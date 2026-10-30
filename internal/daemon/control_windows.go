//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

func Control(action, configPath string) error {
	switch action {
	case "install", "uninstall", "start", "stop", "status":
	default:
		return errors.New("unknown service action")
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Service Control Manager: %w", err)
	}
	defer m.Disconnect()
	if action == "install" {
		return install(m, configPath)
	}
	s, err := m.OpenService(ServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		if action == "uninstall" || action == "stop" {
			return nil
		}
		if action == "status" {
			return ErrNotRunning
		}
	}
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	switch action {
	case "status":
		status, err := s.Query()
		if err != nil {
			return err
		}
		if status.State != svc.Running {
			return ErrNotRunning
		}
		return nil
	case "stop":
		return stopService(s)
	case "start":
		status, err := s.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Running {
			return nil
		}
		if err := s.Start(); err != nil {
			return err
		}
		return waitFor(s, svc.Running)
	case "uninstall":
		if err := stopService(s); err != nil {
			return err
		}
		if err := s.Delete(); err != nil {
			return err
		}
		if err := eventlog.Remove(ServiceName); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return err
		}
	}
	return nil
}

func install(m *mgr.Mgr, configPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return err
	}
	// Quote each argument using Windows command-line rules, including paths with spaces.
	command := windows.ComposeCommandLine([]string{exe, "-config", configPath})
	s, err := m.OpenService(ServiceName)
	if err == nil {
		defer s.Close()
		status, err := s.Query()
		if err != nil {
			return err
		}
		if status.State != svc.Stopped {
			return errors.New("stop the service before updating its registration")
		}
		cfg, err := s.Config()
		if err != nil {
			return err
		}
		cfg.BinaryPathName = command
		cfg.StartType = mgr.StartAutomatic
		cfg.DelayedAutoStart = true
		if err := s.UpdateConfig(cfg); err != nil {
			return err
		}
	} else if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		s, err = m.CreateService(ServiceName, exe, mgr.Config{DisplayName: "PowerChute Prometheus Exporter", Description: "Exports PowerChute Energy Reporting and Data Log metrics for Prometheus.", StartType: mgr.StartAutomatic, DelayedAutoStart: true, ServiceStartName: `NT AUTHORITY\LocalService`}, "-config", configPath)
		if err != nil {
			return err
		}
		defer s.Close()
	} else {
		return err
	}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 30 * time.Second}, {Type: mgr.ServiceRestart, Delay: 60 * time.Second}}, 300); err != nil {
		return err
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return err
	}
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\EventLog\Application\`+ServiceName, registry.READ)
	if err == nil {
		key.Close()
		return nil
	}
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return err
	}
	return eventlog.InstallAsEventCreate(ServiceName, eventlog.Info|eventlog.Warning|eventlog.Error)
}

func stopService(s *mgr.Service) error {
	status, err := s.Query()
	if err != nil {
		return err
	}
	if status.State == svc.Stopped {
		return nil
	}
	if status.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil {
			return err
		}
	}
	return waitFor(s, svc.Stopped)
}

func waitFor(s *mgr.Service, want svc.State) error {
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		status, err := s.Query()
		if err != nil {
			return err
		}
		if status.State == want {
			return nil
		}
		if want == svc.Running && status.State == svc.Stopped {
			return errors.New("service exited during startup; inspect the Application event log")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("service state transition timed out")
}
