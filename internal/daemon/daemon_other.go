//go:build !windows

package daemon

import (
	"context"
	"errors"
)

func Run(ctx context.Context, runner Runner) int { return runner(ctx, func() {}, consoleLogger()) }

func Control(action, configPath string) error {
	return errors.New("use systemctl to manage the Linux service")
}
