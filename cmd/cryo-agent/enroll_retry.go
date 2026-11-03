package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent"
)

// enrollRetryEnvVar is the operator and test knob for the enrollment retry
// budget (agent.EnrollRetryBudget, 3 minutes by default), read by
// "cryo-agent enroll" and "cryo-agent msi-configure":
//
//	unset or empty   the default budget
//	0                a single attempt, no retry (a deliberately unreachable
//	                 console: tests, CI)
//	1 to 600         that many seconds of retrying
//
// Anything else (not a whole number, negative, above 600) is ignored with a
// warning on stderr and the default applies. The value is a number of seconds,
// never a secret, and is not echoed.
const enrollRetryEnvVar = "CRYO_ENROLL_RETRY_SECONDS"

// enrollRetryMaxSeconds bounds the knob: an enrollment must end.
const enrollRetryMaxSeconds = 600

// enrollRetrySettings reads the knob and returns the value for
// agent.EnrollOptions.RetryBudget (0 default, negative single attempt) and the
// context deadline for the whole enrollment: the budget plus the same
// allowance for the last attempt that agent.EnrollTimeout includes.
func enrollRetrySettings(stderr io.Writer) (budget, timeout time.Duration) {
	timeout = agent.EnrollTimeout
	v := strings.TrimSpace(os.Getenv(enrollRetryEnvVar))
	if v == "" {
		return 0, timeout
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > enrollRetryMaxSeconds {
		fmt.Fprintf(stderr, "warning: $%s is not a whole number of seconds from 0 to %d; ignored, the default retry budget applies\n", enrollRetryEnvVar, enrollRetryMaxSeconds)
		return 0, timeout
	}
	extra := agent.EnrollTimeout - agent.EnrollRetryBudget
	if n == 0 {
		return -1, extra
	}
	return time.Duration(n) * time.Second, time.Duration(n)*time.Second + extra
}
