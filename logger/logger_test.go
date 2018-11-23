package logger

import (
	"os"
	"testing"
	"time"
)

func TestNewStandardLogger(t *testing.T) {
	logF := NewStandardLogger(os.Stderr)
	logF.Debugf("time is %v", time.Now())
	logF.Infof("time is %v", time.Now())
	logF.Warnf("time is %v", time.Now())
	logF.Errorf("time is %v", time.Now())
}

func TestNewVerboseLogger(t *testing.T) {
	logF := NewVerboseLogger(os.Stderr)
	logF.Debugf("time is %v", time.Now())
	logF.Infof("time is %v", time.Now())
	logF.Warnf("time is %v", time.Now())
	logF.Errorf("time is %v", time.Now())
}
