package core

import (
	"errors"
	"strconv"
	"syscall"
	"testing"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

func TestReadBackoffNeverTreatsZeroAsUnlimited(t *testing.T) {
	for _, configuredAttempts := range []int{0, -1} {
		t.Run(strconv.Itoa(configuredAttempts), func(t *testing.T) {
			flags := cfg.DefaultFlags()
			flags.ReadRetryAttempts = configuredAttempts
			flags.ReadRetryInterval = 0

			attempts := 0
			err := ReadBackoff(flags, func(attempt int) error {
				attempts = attempt
				return syscall.EAGAIN
			})

			if !errors.Is(err, syscall.EAGAIN) {
				t.Fatalf("error = %v, want EAGAIN", err)
			}
			if attempts != cfg.DefaultReadRetryAttempts {
				t.Fatalf("attempts = %d, want finite default %d", attempts, cfg.DefaultReadRetryAttempts)
			}
		})
	}
}

func TestDefaultFlagsBoundOriginReads(t *testing.T) {
	flags := cfg.DefaultFlags()
	if flags.ReadRetryAttempts != cfg.DefaultReadRetryAttempts {
		t.Fatalf("read retry attempts = %d, want %d", flags.ReadRetryAttempts, cfg.DefaultReadRetryAttempts)
	}
	if flags.ReadRetryInterval <= 0 || flags.ReadRetryMultiplier <= 0 || flags.ReadRetryMax <= 0 {
		t.Fatalf("read retry backoff is not fully initialized: interval=%v multiplier=%v max=%v",
			flags.ReadRetryInterval, flags.ReadRetryMultiplier, flags.ReadRetryMax)
	}
}
