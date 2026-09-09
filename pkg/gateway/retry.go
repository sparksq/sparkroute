// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/routing"
)

type RetrySleeper interface {
	Sleep(ctx context.Context, delay time.Duration) error
}

type timerRetrySleeper struct{}

func (timerRetrySleeper) Sleep(
	ctx context.Context,
	delay time.Duration,
) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type retryStopReason string

const (
	retryAllowed        retryStopReason = ""
	retryPolicyDisabled retryStopReason = "policy_disabled"
	retryBudgetDenied   retryStopReason = "budget_exhausted"
	retryDeadline       retryStopReason = "deadline_exhausted"
)

type retryReservation struct {
	lease  routing.RetryBudgetLease
	delay  time.Duration
	reason string
}

func reserveRetry(
	ctx context.Context,
	request routing.RetryBudgetRequest,
	policy config.EffectiveRetryPolicy,
	retryNumber int,
	retryAfter string,
	reason string,
	picker routing.WeightedPicker,
) (*retryReservation, retryStopReason, error) {
	if policy.Disabled {
		return nil, retryPolicyDisabled, nil
	}
	delay, err := retryDelay(
		policy,
		retryNumber,
		retryAfter,
		time.Now(),
		picker,
	)
	if err != nil {
		return nil, retryPolicyDisabled, err
	}
	if deadline, exists := ctx.Deadline(); exists {
		remaining := time.Until(deadline)
		if remaining <= 0 || delay >= remaining {
			return nil, retryDeadline, nil
		}
	}
	lease := request.TryAcquireRetry()
	if lease == nil {
		return nil, retryBudgetDenied, nil
	}
	return &retryReservation{
		lease:  lease,
		delay:  delay,
		reason: reason,
	}, retryAllowed, nil
}

func retryDelay(
	policy config.EffectiveRetryPolicy,
	retryNumber int,
	retryAfter string,
	now time.Time,
	picker routing.WeightedPicker,
) (time.Duration, error) {
	if retryNumber < 1 {
		retryNumber = 1
	}
	backoff := policy.BaseBackoff
	for current := 1; current < retryNumber; current++ {
		if backoff >= policy.MaxBackoff ||
			backoff > policy.MaxBackoff/2 {
			backoff = policy.MaxBackoff
			break
		}
		backoff *= 2
	}
	if backoff > policy.MaxBackoff {
		backoff = policy.MaxBackoff
	}
	if picker == nil {
		picker = routing.CryptoPicker{}
	}
	jitterLimit := int64(backoff) + 1
	jitter, err := picker.Pick(jitterLimit)
	if err != nil {
		return 0, err
	}
	delay := time.Duration(jitter)
	if upstreamDelay, valid := parseRetryAfter(retryAfter, now); valid {
		if upstreamDelay > policy.MaxRetryAfter {
			upstreamDelay = policy.MaxRetryAfter
		}
		if upstreamDelay > delay {
			delay = upstreamDelay
		}
	}
	return delay, nil
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		maxSeconds := int64(^uint64(0)>>1) / int64(time.Second)
		if seconds < 0 || seconds > maxSeconds {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func retryFailureClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "retry_deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "retry_delay_cancelled"
	default:
		return "retry_delay_error"
	}
}
