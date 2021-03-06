package dlog

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Severity int32

type globals struct {
	sync.Mutex
	logLevel       Severity
	useSyslog      *bool
	appName        string
	syslogFacility string
	systemLogger   *systemLogger
	fileName       *string
	outFd          *os.File
	lastMessage    string
	lastOccurrence time.Time
	occurrences    uint64
}

var (
	_globals = globals{
		appName:        "-",
		lastMessage:    "",
		lastOccurrence: time.Now(),
		occurrences:    0,
	}
)

const (
	SeverityDebug Severity = iota
	SeverityInfo
	SeverityNotice
	SeverityWarning
	SeverityError
)

const (
	floodDelay      = 5 * time.Second
	floodMinRepeats = 3
)

var SeverityName = []string{
	SeverityDebug:    "DEBUG",
	SeverityInfo:     "INFO",
	SeverityNotice:   "NOTICE",
	SeverityWarning:  "WARNING",
	SeverityError:    "ERROR",
}

func Debugf(format string, args ...interface{}) {
	logf(SeverityDebug, format, args...)
}

func Infof(format string, args ...interface{}) {
	logf(SeverityInfo, format, args...)
}

func Noticef(format string, args ...interface{}) {
	logf(SeverityNotice, format, args...)
}

func Warnf(format string, args ...interface{}) {
	logf(SeverityWarning, format, args...)
}

type errorString string
func (e errorString) Error() string {
	return string(e)
}

func Errorf(format string, args ...interface{}) error {
	msg := errorString(*logf(SeverityError, format, args...))
	return msg
}

func Debug(message interface{}) {
	log(SeverityDebug, message)
}

func Info(message interface{}) {
	log(SeverityInfo, message)
}

func Notice(message interface{}) {
	log(SeverityNotice, message)
}

func Warn(message interface{}) {
	log(SeverityWarning, message)
}

func Error(message interface{}) {
	log(SeverityError, message)
}


func (s *Severity) get() Severity {
	return Severity(atomic.LoadInt32((*int32)(s)))
}

func (s *Severity) set(val Severity) {
	atomic.StoreInt32((*int32)(s), int32(val))
}

func (s *Severity) String() string {
	return strconv.FormatInt(int64(*s), 10)
}

func (s *Severity) Get() interface{} {
	return s.get()
}

func (s *Severity) Set(strVal string) error {
	val, _ := strconv.Atoi(strVal)
	s.set(Severity(val))
	return nil
}

func Init(appName string, logLevel Severity, syslogFacility string) error {
	_globals.logLevel.set(logLevel)

	if len(syslogFacility) == 0 {
		syslogFacility = "DAEMON"
	}
	_globals.appName = appName
	_globals.syslogFacility = syslogFacility
	_globals.useSyslog = flag.Bool("syslog", false, "Send logs to the local system logger (Eventlog on Windows, syslog on Unix)")
	_globals.fileName = flag.String("logfile", "", "Write logs to file")
	flag.Var(&_globals.logLevel, "loglevel", fmt.Sprintf("Log level (%d-%d)", SeverityDebug, SeverityError))
	return nil
}

func LogLevel() Severity {
	_globals.Lock()
	logLevel := _globals.logLevel.get()
	_globals.Unlock()
	return logLevel
}

func SetLogLevel(logLevel Severity) {
	_globals.Lock()
	_globals.logLevel.set(logLevel)
	_globals.Unlock()
}

func UseSyslog(value bool) {
	_globals.Lock()
	_globals.useSyslog = &value
	_globals.Unlock()
}

func UseLogFile(fileName string) {
	_globals.Lock()
	_globals.fileName = &fileName
	_globals.Unlock()
}

func GetFileDescriptor() (*os.File) {
	_globals.Lock()
	createFileDescriptor()
	_globals.Unlock()
	return _globals.outFd
}

func SetFileDescriptor(fd *os.File) {
	_globals.Lock()
	_globals.outFd = fd
	_globals.Unlock()
}

func createFileDescriptor() {
	if _globals.fileName != nil && len(*_globals.fileName) > 0 && _globals.outFd == nil {
		outFd, err := os.OpenFile(*_globals.fileName, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
		if err == nil {
			_globals.outFd = outFd
		}
	}
}

func logf(severity Severity, format string, args ...interface{}) *string {
	if severity < _globals.logLevel.get() {
		return nil
	}
	now := time.Now().Local()
	year, month, day := now.Date()
	hour, minute, second := now.Clock()
	message := fmt.Sprintf(format, args...)
	message = strings.TrimSpace(strings.TrimSuffix(message, "\n"))
	if len(message) <= 0 {
		return nil
	}
	_globals.Lock()
	defer _globals.Unlock()
	if _globals.lastMessage == message {
		if time.Since(_globals.lastOccurrence) < floodDelay {
			_globals.occurrences++
			if _globals.occurrences > floodMinRepeats {
				return nil
			}
		}
	} else {
		_globals.occurrences = 0
		_globals.lastMessage = message
	}
	_globals.lastOccurrence = now
	if *_globals.useSyslog && _globals.systemLogger == nil {
		systemLogger, err := newSystemLogger(_globals.appName, _globals.syslogFacility)
		if err == nil {
			_globals.systemLogger = systemLogger
		}
	}
	createFileDescriptor()
	if _globals.systemLogger != nil {
		(*_globals.systemLogger).writeString(severity, message)
	} else {
		line := fmt.Sprintf("[%d-%02d-%02d %02d:%02d:%02d] [%s] %s\n", year, int(month), day, hour, minute, second, SeverityName[severity], message)
		if _globals.outFd != nil {
			_globals.outFd.WriteString(line)
			_globals.outFd.Sync()
		} else {
			os.Stderr.WriteString(line)
		}
		return &message
	}
	return nil
}

func log(severity Severity, args interface{}) {
	logf(severity, "%v", args)
}
