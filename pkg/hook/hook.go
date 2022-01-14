/*
Copyright 2021 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hook

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/git-sync/pkg/logging"
)

var (
	hookRunCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "git_sync_hook_run_count_total",
		Help: "How many hook runs completed, partitioned by name and state (success, error)",
	}, []string{"name", "status"})
)

// Describes what a Hook needs to implement, run by HookRunner
type Hook interface {
	// Describes hook
	Name() string
	// Function that called by HookRunner
	Do(ctx context.Context, hash string) error
}

type hookData struct {
	ch    chan struct{}
	mutex sync.Mutex
	hash  string
}

// NewHookData returns a new HookData
func NewHookData() *hookData {
	return &hookData{
		ch: make(chan struct{}, 1),
	}
}

func (d *hookData) events() chan struct{} {
	return d.ch
}

func (d *hookData) get() string {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	return d.hash
}

func (d *hookData) set(newHash string) {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.hash = newHash
}

func (d *hookData) send(newHash string) {
	d.set(newHash)

	// Non-blocking write.  If the channel is full, the consumer will see the
	// newest value.  If the channel was not full, the consumer will get another
	// event.
	select {
	case d.ch <- struct{}{}:
	default:
	}
}

// NewHookRunner returns a new HookRunner
func NewHookRunner(hook Hook, backoff time.Duration, data *hookData, log *logging.Logger, hasSucceededOnce chan bool) *HookRunner {
	return &HookRunner{hook: hook, backoff: backoff, data: data, logger: log, hasCompletedOnce: hasSucceededOnce}
}

// HookRunner struct
type HookRunner struct {
	// Hook to run and check
	hook Hook
	// Backoff for failed hooks
	backoff time.Duration
	// Holds the data as it crosses from producer to consumer.
	data *hookData
	// Logger
	logger *logging.Logger
	// hasCompletedOnce is used to send true if and only if first run executed
	// successfully and false otherwise to some receiver.  Should be
	// initialised to a buffered channel of size 1.
	// Is only meant for use within sendCompletedOnceMessageIfApplicable.
	hasCompletedOnce chan bool
}

// Send sends hash to hookdata
func (r *HookRunner) Send(hash string) {
	r.data.send(hash)
}

// Run waits for trigger events from the channel, and run hook when triggered
func (r *HookRunner) Run(ctx context.Context) {
	var lastHash string
	prometheus.MustRegister(hookRunCount)

	// Wait for trigger from hookData.Send
	for range r.data.events() {
		// Retry in case of error
		for {
			// Always get the latest value, in case we fail-and-retry and the
			// value changed in the meantime.  This means that we might not send
			// every single hash.
			hash := r.data.get()
			if hash == lastHash {
				break
			}

			if err := r.hook.Do(ctx, hash); err != nil {
				r.logger.Error(err, "hook failed")
				updateHookRunCountMetric(r.hook.Name(), "error")
				// don't want to sleep unnecessarily terminating anyways
				r.sendCompletedOnceMessageIfApplicable(false)
				time.Sleep(r.backoff)
			} else {
				updateHookRunCountMetric(r.hook.Name(), "success")
				lastHash = hash
				r.sendCompletedOnceMessageIfApplicable(true)
				break
			}
		}
	}
}

// If hasCompletedOnce is nil, does nothing. Otherwise, forwards the caller
// provided success status (as a boolean) of HookRunner to receivers of
// hasCompletedOnce, closes said chanel, and terminates this goroutine.
// Using this function to write to hasCompletedOnce ensures it is only ever
// written to once.
func (r *HookRunner) sendCompletedOnceMessageIfApplicable(completedSuccessfully bool) {
	if r.hasCompletedOnce != nil {
		r.hasCompletedOnce <- completedSuccessfully
		close(r.hasCompletedOnce)
		runtime.Goexit()
	}
}

// WaitForCompletion waits for HookRunner to send completion message to
// calling thread and returns either true if HookRunner executed successfully
// and some error otherwise.
// Assumes that r.hasCompletedOnce is not nil, but if it is, returns an error
func (r *HookRunner) WaitForCompletion() error {
	// Make sure function should be called
	if r.hasCompletedOnce == nil {
		return fmt.Errorf("HookRunner.WaitForCompletion called on async runner")
	}

	// Perform wait on HookRunner
	exechookChannelFinishedSuccessfully := <-r.hasCompletedOnce
	if !exechookChannelFinishedSuccessfully {
		return fmt.Errorf("exechook completed with error")
	}

	return nil
}

func updateHookRunCountMetric(name, status string) {
	hookRunCount.WithLabelValues(name, status).Inc()
}
