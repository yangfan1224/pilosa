// Copyright 2017 Pilosa Corp.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logger

import (
	"fmt"
	"io"
	"log"
)

type LogLevel int

const (
	DEBUG = LogLevel(1)
	INFO  = LogLevel(2)
	WARN  = LogLevel(3)
	ERROR = LogLevel(4)
	FATAL = LogLevel(5)
)

func (l LogLevel) String() string {
	switch l {
	case 1:
		return "DEBUG"
	case 2:
		return "INFO"
	case 3:
		return "WARNING"
	case 4:
		return "ERROR"
	case 5:
		return "FATAL"
	}
	panic("invalid LogLevel")
}

func Logf(logger log.Logger, cfgLevel LogLevel, msgLevel LogLevel, f string, args ...interface{}) {
	if cfgLevel > msgLevel {
		return
	}
	logger.Output(3, fmt.Sprintf(msgLevel.String()+": "+f, args...))
}

// Ensure nopLogger implements interface.
var _ Logger = &nopLogger{}

// Logger represents an interface for a shared logger.
type Logger interface {
	Printf(format string, v ...interface{})
	Debugf(format string, v ...interface{})
	Infof(format string, v ...interface{})
	Warnf(format string, v ...interface{})
	Errorf(format string, v ...interface{})
}

// NopLogger represents a Logger that doesn't do anything.
var NopLogger Logger = &nopLogger{}

type nopLogger struct{}

func (n *nopLogger) Warnf(format string, v ...interface{}) {
}

func (n *nopLogger) Infof(format string, v ...interface{}) {
}

func (n *nopLogger) Errorf(format string, v ...interface{}) {
}

// Printf is a no-op implementation of the Logger Printf method.
func (n *nopLogger) Printf(format string, v ...interface{}) {}

// Debugf is a no-op implementation of the Logger Debugf method.
func (n *nopLogger) Debugf(format string, v ...interface{}) {}

// standardLogger is a basic implementation of Logger based on log.Logger.
type standardLogger struct {
	logger *log.Logger
	logLevel LogLevel
}

func NewStandardLogger(w io.Writer) *standardLogger {
	return &standardLogger{
		logger: log.New(w, "[pilosa]", log.LstdFlags),
		logLevel: INFO,
	}
}

func (s *standardLogger) Printf(format string, v ...interface{}) {
	//s.logger.Printf(format, v...)
	Logf(*s.logger, s.logLevel, DEBUG, format, v...)
}

func (s *standardLogger) Debugf(format string, v ...interface{}) {
	Logf(*s.logger, s.logLevel, DEBUG, format, v...)
}

func (s *standardLogger) Infof(format string, v ...interface{}) {
	Logf(*s.logger, s.logLevel, INFO,  format, v...)
}

func (s *standardLogger) Warnf(format string, v ...interface{}) {
	Logf(*s.logger, s.logLevel, WARN,  format, v...)
}

func (s *standardLogger) Errorf(format string, v ...interface{}) {
	Logf(*s.logger, s.logLevel, ERROR,  format, v...)
}


func (s *standardLogger) Logger() *log.Logger {
	return s.logger
}

// verboseLogger is an implementation of Logger which includes debug messages.
type verboseLogger struct {
	logger *log.Logger
	logLevel LogLevel
}

func NewVerboseLogger(w io.Writer) *verboseLogger {
	return &verboseLogger{
		logger: log.New(w, "[pilosa]", log.LstdFlags),
		logLevel: DEBUG,
	}
}

func (vb *verboseLogger) Printf(format string, v ...interface{}) {
	Logf(*vb.logger, vb.logLevel, DEBUG, format, v...)
	//vb.logger.Printf(format, v...)
}

func (vb *verboseLogger) Debugf(format string, v ...interface{}) {
	Logf(*vb.logger, vb.logLevel, DEBUG,  format, v...)
	//vb.logger.Printf(format, v...)
}

func (vb *verboseLogger) Logger() *log.Logger {
	return vb.logger
}

func (vb *verboseLogger) Infof(format string, v ...interface{}) {
	Logf(*vb.logger, vb.logLevel, INFO,  format, v...)
}

func (vb *verboseLogger) Warnf(format string, v ...interface{}) {
	Logf(*vb.logger, vb.logLevel, WARN,  format, v...)
}

func (vb *verboseLogger) Errorf(format string, v ...interface{}) {
	Logf(*vb.logger, vb.logLevel, ERROR,  format, v...)
}

