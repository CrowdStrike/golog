package logger

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"unsafe"
)

// The csyslog function is necessary here because cgo does not appear
// to be able to call a variadic function directly and syslog has the
// same signature as printf.

// #include <stdlib.h>
// #include <syslog.h>
// void csyslog(int p, const char *m) {
//     syslog(p, "%s", m);
// }
import "C"

const (
	NumMessages    = 10 * 1024 // number of allowed log messages
	MaxFreeMsgSize = 8 * 1024  // maximum size of a free pooled msg
)

// container for a pending log message
type logMessage struct {
	bytes.Buffer
	level C.int
}

var (
	ErrLogFullBuf           = errors.New("Log message queue is full")
	ErrFreeMessageOverflow  = errors.New("Too many free messages. Overflow of fixed	set.")
	ErrFreeMessageUnderflow = errors.New("Too few free messages. Underflow of fixed	set.")
	ErrUnavailable          = errors.New("Log system is shut down.")

	// the logName object for syslog to use
	logName *C.char

	// the message queue of pending or free messages
	// since only one can be full at a time, the total size will be about 10MB
	messages     chan *logMessage = make(chan *logMessage, NumMessages)
	freeMessages chan *logMessage = make(chan *logMessage, NumMessages)

	// mapping of our levels to syslog values
	levelSysLog = map[Level]C.int{
		Levels.Access: C.LOG_INFO,
		Levels.Off:    C.LOG_DEBUG,
		Levels.Panic:  C.LOG_ERR,
		Levels.Error:  C.LOG_ERR,
		Levels.Warn:   C.LOG_WARNING,
		Levels.Info:   C.LOG_INFO,
		Levels.Debug:  C.LOG_DEBUG,
	}

	// mirror of levelMap used to avoid making a new string with '[]' on every log
	// call
	levelMapFmt = map[Level][]byte{
		Levels.Access: []byte("[Access] "),
		Levels.Off:    []byte("[Off] "),
		Levels.Panic:  []byte("[Panic] "),
		Levels.Error:  []byte("[Error] "),
		Levels.Warn:   []byte("[Warn] "),
		Levels.Info:   []byte("[Info] "),
		Levels.Debug:  []byte("[Debug] "),
	}

	customSock      net.Conn = nil
	shuttingDownMux sync.RWMutex
	shuttingDown    bool
	wg              sync.WaitGroup
)

// When called, this will switch over to writting log messages to the defined socket.
func SetCustomSocket(address, network string) (err error) {
	customSock, err = net.Dial(network, address)

	return err
}

// SetLogName sets the indentifier used by syslog for this program
func SetLogName(p string) (err error) {
	if logName != nil {
		C.free(unsafe.Pointer(logName))
	}
	logName = C.CString(p)
	_, err = C.openlog(logName, C.LOG_NDELAY|C.LOG_NOWAIT|C.LOG_PID, C.LOG_USER)
	if err != nil {
		atomic.AddUint64(&errCount, 1)
	}

	return err
}

// freeMsg releases the message back to be reused
func freeMsg(msg *logMessage) (err error) {
	select {
	case freeMessages <- msg: // no-op
	default:
		atomic.AddUint64(&errCount, 1)
		return ErrFreeMessageOverflow
	}

	return
}

// queueMsgDirect adds a message to the pending messages channel. It will drop the
// message and return an error if the channel is full.
func queueMsgDirect(logEntry string) (err error) {

	shuttingDownMux.RLock()
	defer shuttingDownMux.RUnlock()

	if !shuttingDown {

		atomic.AddUint64(&logCount, 1)

		var msg *logMessage

		// get a message if possible
		select {
		case msg = <-freeMessages:
			defer func() {
				if err != nil {
					freeMsg(msg)
				}
			}()
		default:
			// no messages left, drop
			atomic.AddUint64(&dropCount, 1)
			return
		}

		// render the message: level prefix, message body, C null terminator
		msg.level = levelSysLog[Levels.Debug]
		if msg.Write([]byte(logEntry)); err != nil {
			atomic.AddUint64(&errCount, 1)
			return
		}
		if msg.WriteByte(0); err != nil {
			atomic.AddUint64(&errCount, 1)
			return
		}

		// queue the message
		select {
		case messages <- msg:
			// no-op
		default:
			// this should never happen since there is an exact number of messages
			atomic.AddUint64(&errCount, 1)
			return ErrLogFullBuf
		}
	} else {
		return ErrUnavailable
	}

	return
}

// queueMsg adds a message to the pending messages channel. It will drop the
// message and return an error if the channel is full.
func queueMsg(lvl Level, prefix, format string, v ...interface{}) (err error) {
	logMsg := fmt.Sprintf("%s %s %s", levelMapFmt[lvl], prefix, fmt.Sprintf(format, v...))
	return queueMsgDirect(logMsg)
}

// write a message to syslog. This is a concrete, blocking event.
func write(msg *logMessage) (err error) {
	start := (*C.char)(unsafe.Pointer(&msg.Bytes()[0]))
	if _, err = C.csyslog(C.LOG_USER|msg.level, start); err != nil {
		atomic.AddUint64(&errCount, 1)
	}
	return
}

// write a message to a pre-defined custom socket. This is a concrete, blocking event.
// Writes out using the syslog rfc5424 format.
func writeCustomSocket(msg *logMessage) (err error) {
	if _, err = customSock.Write(bytes.Join([][]byte{[]byte(fmt.Sprintf("<%d>", C.LOG_USER|msg.level)),
		msg.Bytes()}, []byte(""))); err != nil {
		atomic.AddUint64(&errCount, 1)
	}
	return
}

// logWriter will write out messages to syslog. It may block if something breaks
// within the syslog call.
func logWriter() {
	for msg := range messages {
		if customSock == nil {
			write(msg)
		} else {
			writeCustomSocket(msg)
		}

		if msg.Cap() > MaxFreeMsgSize {
			*msg = logMessage{}
		} else {
			msg.Reset()
		}
		freeMsg(msg)
	}
	if customSock != nil {
		customSock.Close()
	}
}

func init() {
	msgArr := make([]logMessage, NumMessages)
	for i := range msgArr {
		if err := freeMsg(&msgArr[i]); err != nil {
			break
		}
	}

	wg.Add(1)
	go func() {
		logWriter()
		wg.Done()
	}()
}

// closing up shop - flush the queue
func drainTheQueue() {
	// exclusively acquire the mux
	shuttingDownMux.Lock()
	// set the flag
	shuttingDown = true
	// close the channel
	close(messages)
	// release the mux
	shuttingDownMux.Unlock()

	// wait for logwriter to close
	wg.Wait()

}
