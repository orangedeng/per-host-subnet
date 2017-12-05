//+build windows
package register

import (
	"os"
	"time"
	"unsafe"

	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/debug"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// credits for https://github.com/docker/docker/blob/5c1826ec4de381df9f739ce0de28e37d4f734d47/cmd/dockerd/service_windows.go

const (
	ServiceName      = "rancher-per-host-subnet"
	StartupScript    = "startup_per-host-subnet.ps1"
	logFile          = "C:/ProgramData/rancher/per-host-subnet.log"
	rancherPanicFile = "C:/ProgramData/rancher/per-host-subnet_panic.log"
	homeDir          = "C:/ProgramData/rancher"
	// These should match the values in event_messages.mc.
	eventInfo  = 1
	eventWarn  = 1
	eventError = 1
	eventDebug = 2
	eventPanic = 3

	eventFatal = 4

	eventExtraOffset = 10 // Add this to any event to get a string that supports extended data
)

var (
	service       *handler
	setStdHandle  = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetStdHandle")
	oldStderr     syscall.Handle
	panicFile     *os.File
	serviceSignal = make(chan bool)
)

type handler struct {
	tosvc   chan bool
	fromsvc chan error
}

type etwHook struct {
	log *eventlog.Log
}

func (h *etwHook) Levels() []logrus.Level {
	return []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
		logrus.WarnLevel,
		logrus.InfoLevel,
		logrus.DebugLevel,
	}
}

func (h *etwHook) Fire(e *logrus.Entry) error {
	var (
		etype uint16
		eid   uint32
	)

	switch e.Level {
	case logrus.PanicLevel:
		etype = windows.EVENTLOG_ERROR_TYPE
		eid = eventPanic
	case logrus.FatalLevel:
		etype = windows.EVENTLOG_ERROR_TYPE
		eid = eventFatal
	case logrus.ErrorLevel:
		etype = windows.EVENTLOG_ERROR_TYPE
		eid = eventError
	case logrus.WarnLevel:
		etype = windows.EVENTLOG_WARNING_TYPE
		eid = eventWarn
	case logrus.InfoLevel:
		etype = windows.EVENTLOG_INFORMATION_TYPE
		eid = eventInfo
	case logrus.DebugLevel:
		etype = windows.EVENTLOG_INFORMATION_TYPE
		eid = eventDebug
	default:
		return errors.New("unknown level")
	}

	// If there is additional data, include it as a second string.
	exts := ""
	if len(e.Data) > 0 {
		fs := bytes.Buffer{}
		for k, v := range e.Data {
			fs.WriteString(k)
			fs.WriteByte('=')
			fmt.Fprint(&fs, v)
			fs.WriteByte(' ')
		}

		exts = fs.String()[:fs.Len()-1]
		eid += eventExtraOffset
	}

	if h.log == nil {
		fmt.Fprintf(os.Stderr, "%s [%s]\n", e.Message, exts)
		return nil
	}

	var (
		ss  [2]*uint16
		err error
	)

	ss[0], err = syscall.UTF16PtrFromString(e.Message)
	if err != nil {
		return err
	}

	count := uint16(1)
	if exts != "" {
		ss[1], err = syscall.UTF16PtrFromString(exts)
		if err != nil {
			return err
		}

		count++
	}

	return windows.ReportEvent(h.log.Handle, etype, 0, eid, 0, count, 0, &ss[0], nil)
}

func getServicePath() (string, error) {
	p, err := exec.LookPath(os.Args[0])
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

func registerService() error {
	p, err := getServicePath()
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	c := mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
		DisplayName:  "Rancher Per-host-subnet",
	}

	// Configure the service to launch with the arguments that were just passed.
	args := []string{"--enable-route-update"}

	s, err := m.CreateService(ServiceName, p, c, args...)
	if err != nil {
		return err
	}
	defer s.Close()

	// See http://stackoverflow.com/questions/35151052/how-do-i-configure-failure-actions-of-a-windows-service-written-in-go
	const (
		scActionNone       = 0
		scActionRestart    = 1
		scActionReboot     = 2
		scActionRunCommand = 3

		serviceConfigFailureActions = 2
	)

	type serviceFailureActions struct {
		ResetPeriod  uint32
		RebootMsg    *uint16
		Command      *uint16
		ActionsCount uint32
		Actions      uintptr
	}

	type scAction struct {
		Type  uint32
		Delay uint32
	}
	t := []scAction{
		{Type: scActionRestart, Delay: uint32(60 * time.Second / time.Millisecond)},
		{Type: scActionRestart, Delay: uint32(60 * time.Second / time.Millisecond)},
		{Type: scActionNone},
	}
	lpInfo := serviceFailureActions{ResetPeriod: uint32(24 * time.Hour / time.Second), ActionsCount: uint32(3), Actions: uintptr(unsafe.Pointer(&t[0]))}
	err = windows.ChangeServiceConfig2(s.Handle, serviceConfigFailureActions, (*byte)(unsafe.Pointer(&lpInfo)))
	if err != nil {
		return err
	}

	err = eventlog.Install(ServiceName, p, false, eventlog.Info|eventlog.Warning|eventlog.Error)
	if err != nil {
		return err
	}

	return nil
}

func unregisterService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return err
	}
	defer s.Close()

	eventlog.Remove(ServiceName)
	err = s.Delete()
	if err != nil {
		return err
	}
	return nil
}

func initService(register, unregister bool) error {
	if _, err := os.Stat(homeDir); err != nil {
		err = os.MkdirAll(homeDir, 0755)
		if err != nil {
			return err
		}
	}
	if register {
		err := registerService()
		if err != nil {
			logrus.Fatalf("Failed to register service, err: %v", err)
		}
		os.Exit(0)
	}
	if unregister {
		err := unregisterService()
		if err != nil {
			logrus.Fatalf("Failed to unregister service, err: %v", err)
		}
		os.Exit(0)
	}
	interactive, err := svc.IsAnInteractiveSession()
	if err != nil {
		return err
	}

	h := &handler{
		tosvc:   make(chan bool),
		fromsvc: make(chan error),
	}

	var log *eventlog.Log
	if !interactive {
		log, err = eventlog.Open(ServiceName)
		if err != nil {
			return err
		}
	}

	logrus.AddHook(&etwHook{log})
	if _, err := os.Stat(logFile); err != nil {
		_, err := os.Create(logFile)
		if err != nil {
			return err
		}
	}
	file, err := os.OpenFile(logFile, os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	logrus.SetOutput(file)

	service = h
	go func() {
		if interactive {
			err = debug.Run(ServiceName, h)
		} else {
			err = svc.Run(ServiceName, h)
		}

		h.fromsvc <- err
	}()

	// Wait for the first signal from the service handler.
	err = <-h.fromsvc
	if err != nil {
		return err
	}
	return nil
}

func (h *handler) started() error {
	err := initPanicFile(rancherPanicFile)
	if err != nil {
		return err
	}

	h.tosvc <- false
	return nil
}

func (h *handler) stopped(err error) {
	logrus.Debugf("Stopping service: %v", err)
	h.tosvc <- err != nil
	<-h.fromsvc
}

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending, Accepts: 0}
	// Unblock initService()
	h.fromsvc <- nil

	// Wait for initialization to complete.
	failed := <-h.tosvc
	if failed {
		logrus.Debug("Aborting service start due to failure during initialization")
		return true, 1
	}

	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.Accepted(windows.SERVICE_ACCEPT_PARAMCHANGE)}
	logrus.Debug("Service running")
Loop:
	for {
		select {
		case failed = <-h.tosvc:
			break Loop
		case c := <-r:
			switch c.Cmd {
			case svc.Cmd(windows.SERVICE_CONTROL_PARAMCHANGE):
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending, Accepts: 0}
				failed = true
				serviceSignal <- true
				break Loop
			}
		}
	}
	removePanicFile()
	if failed {
		return true, 1
	}
	return false, 0
}

func initPanicFile(path string) error {
	var err error
	_, err = os.Create(rancherPanicFile)
	if err != nil {
		return err
	}
	panicFile, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}

	st, err := panicFile.Stat()
	if err != nil {
		return err
	}

	// If there are contents in the file already, move the file out of the way
	// and replace it.
	if st.Size() > 0 {
		panicFile.Close()
		os.Rename(path, path+".old")
		panicFile, err = os.Create(path)
		if err != nil {
			return err
		}
	}

	// Update STD_ERROR_HANDLE to point to the panic file so that Go writes to
	// it when it panics. Remember the old stderr to restore it before removing
	// the panic file.
	sh := syscall.STD_ERROR_HANDLE
	h, err := syscall.GetStdHandle(sh)
	if err != nil {
		return err
	}

	oldStderr = h

	r, _, err := setStdHandle.Call(uintptr(sh), uintptr(panicFile.Fd()))
	if r == 0 && err != nil {
		return err
	}

	return nil
}

func removePanicFile() {
	if st, err := panicFile.Stat(); err == nil {
		if st.Size() == 0 {
			sh := syscall.STD_ERROR_HANDLE
			setStdHandle.Call(uintptr(sh), uintptr(oldStderr))
			panicFile.Close()
			os.Remove(panicFile.Name())
		}
	}
}

func notifySystem() {
	if service != nil {
		err := service.started()
		if err != nil {
			logrus.Fatal(err)
		}
	}
}

func NotifyShutdown(err error) {
	if service != nil {
		if err != nil {
			logrus.Fatal(err)
		}
		service.stopped(err)
	}
}

func Init(register, unregister bool) error {
	if err := initService(register, unregister); err != nil {
		return err
	}

	notifySystem()

	//listen to service stop signal
	go func() {
		signal := <-serviceSignal
		if signal {
			logrus.Info("Receiving service stop signal. Stopping per-host-subnet")
			os.Exit(0)
		}
	}()

	return nil
}
