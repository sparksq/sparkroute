// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/routing"
)

type maximumPicker struct{}

func (maximumPicker) Pick(limit int64) (int64, error) {
	return limit - 1, nil
}

func TestRetryDelayUsesExponentialBackoffWithFullJitter(t *testing.T) {
	t.Parallel()

	policy := config.RetryPolicy{
		BaseBackoff:   config.Duration(100 * time.Millisecond),
		MaxBackoff:    config.Duration(250 * time.Millisecond),
		MaxRetryAfter: config.Duration(time.Second),
	}.Effective()
	tests := []struct {
		retry int
		want  time.Duration
	}{
		{retry: 1, want: 100 * time.Millisecond},
		{retry: 2, want: 200 * time.Millisecond},
		{retry: 3, want: 250 * time.Millisecond},
		{retry: 20, want: 250 * time.Millisecond},
	}
	for _, test := range tests {
		delay, err := retryDelay(
			policy,
			test.retry,
			"",
			time.Unix(0, 0),
			maximumPicker{},
		)
		if err != nil {
			t.Fatalf("retryDelay(%d) error = %v", test.retry, err)
		}
		if delay != test.want {
			t.Fatalf(
				"retryDelay(%d) = %s, want %s",
				test.retry,
				delay,
				test.want,
			)
		}
	}
}

func TestRetryDelayRespectsAndCapsRetryAfter(t *testing.T) {
	t.Parallel()

	policy := config.RetryPolicy{
		BaseBackoff:   config.Duration(time.Millisecond),
		MaxBackoff:    config.Duration(time.Millisecond),
		MaxRetryAfter: config.Duration(3 * time.Second),
	}.Effective()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		retryAfter string
		want       time.Duration
	}{
		{name: "delta seconds", retryAfter: "2", want: 2 * time.Second},
		{
			name:       "http date",
			retryAfter: now.Add(time.Second).Format(http.TimeFormat),
			want:       time.Second,
		},
		{name: "capped", retryAfter: "86400", want: 3 * time.Second},
		{name: "trimmed", retryAfter: " 1 ", want: time.Second},
		{name: "invalid", retryAfter: "later", want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			delay, err := retryDelay(
				policy,
				1,
				test.retryAfter,
				now,
				zeroPicker{},
			)
			if err != nil {
				t.Fatalf("retryDelay() error = %v", err)
			}
			if delay != test.want {
				t.Fatalf("delay = %s, want %s", delay, test.want)
			}
		})
	}
}

func TestReserveRetryHonorsDeadlineAndBudget(t *testing.T) {
	t.Parallel()

	policy := config.RetryPolicy{
		BaseBackoff: config.Duration(100 * time.Millisecond),
		MaxBackoff:  config.Duration(100 * time.Millisecond),
		Budget: config.RetryBudgetPolicy{
			Ratio:          0.01,
			MinConcurrency: 1,
		},
	}.Effective()
	manager := routing.NewRetryBudgetManager(nil)
	disabledRequest := manager.BeginRequest("disabled", policy.Budget)
	disabledPolicy := policy
	disabledPolicy.Disabled = true
	reservation, stop, err := reserveRetry(
		context.Background(),
		disabledRequest,
		disabledPolicy,
		1,
		"",
		"upstream_http_503",
		zeroPicker{},
	)
	if err != nil || reservation != nil || stop != retryPolicyDisabled {
		t.Fatalf("disabled reservation = %#v, %q, %v", reservation, stop, err)
	}
	disabledRequest.End()

	blockingRequest := manager.BeginRequest("model", policy.Budget)
	blockingLease := blockingRequest.TryAcquireRetry()
	if blockingLease == nil {
		t.Fatal("could not acquire blocking retry")
	}
	request := manager.BeginRequest("model", policy.Budget)

	reservation, stop, err = reserveRetry(
		context.Background(),
		request,
		policy,
		1,
		"",
		"upstream_http_503",
		zeroPicker{},
	)
	if err != nil || reservation != nil || stop != retryBudgetDenied {
		t.Fatalf("budget reservation = %#v, %q, %v", reservation, stop, err)
	}

	blockingLease.Release()
	deadlineContext, cancel := context.WithTimeout(
		context.Background(),
		10*time.Millisecond,
	)
	defer cancel()
	reservation, stop, err = reserveRetry(
		deadlineContext,
		request,
		policy,
		1,
		"",
		"upstream_http_503",
		maximumPicker{},
	)
	if err != nil || reservation != nil || stop != retryDeadline {
		t.Fatalf("deadline reservation = %#v, %q, %v", reservation, stop, err)
	}
	request.End()
	blockingRequest.End()
}
