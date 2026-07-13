package hustleruntime

import (
	"context"
	"sync"
	"testing"

	"github.com/looprig/harness/pkg/hustle"
)

func TestLaneGrantsExecutionInOwnershipFIFOOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		part hustle.Participation
	}{
		{name: "blocking fifo", part: hustle.ParticipationBlocking},
		{name: "background fifo", part: hustle.ParticipationBackground},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			controller, err := New(context.Background(), testConfig())
			if err != nil {
				t.Fatal(err)
			}
			runs := make([]*ownedRun, 0, 3)
			for range 3 {
				run, ownErr := controller.own(context.Background(), testCase.part, noOpFinalizer)
				if ownErr != nil {
					t.Fatal(ownErr)
				}
				runs = append(runs, run)
			}
			if err := runs[0].awaitExecution(); err != nil {
				t.Fatal(err)
			}
			assertNotGranted(t, runs[1], "second grant while first executes")
			assertNotGranted(t, runs[2], "third grant while first executes")
			if err := runs[0].finalize(context.Background(), hustle.Outcome{}); err != nil {
				t.Fatal(err)
			}
			if err := runs[1].awaitExecution(); err != nil {
				t.Fatal(err)
			}
			assertNotGranted(t, runs[2], "third grant before second releases")
			if err := runs[1].finalize(context.Background(), hustle.Outcome{}); err != nil {
				t.Fatal(err)
			}
			if err := runs[2].awaitExecution(); err != nil {
				t.Fatal(err)
			}
			if err := runs[2].finalize(context.Background(), hustle.Outcome{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExecutionSlotReleasesBeforeFinalizerButOwnershipDoesNot(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "slow finalizer keeps total capacity bounded"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			config.Blocking = LaneLimits{Concurrent: 1, Queued: 1}
			controller, err := New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			first, err := controller.own(context.Background(), hustle.ParticipationBlocking, func(context.Context, hustle.Outcome) error {
				close(entered)
				<-release
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			second, err := controller.own(context.Background(), hustle.ParticipationBlocking, noOpFinalizer)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.awaitExecution(); err != nil {
				t.Fatal(err)
			}
			finalized := make(chan error, 1)
			go func() { finalized <- first.finalize(context.Background(), hustle.Outcome{}) }()
			<-entered
			if err := second.awaitExecution(); err != nil {
				t.Fatalf("second did not receive released execution slot: %v", err)
			}
			third, err := controller.own(context.Background(), hustle.ParticipationBlocking, noOpFinalizer)
			if third != nil || err == nil {
				t.Fatalf("third own() = (%v,%v), want full while first finalizes", third, err)
			}
			close(release)
			if err := <-finalized; err != nil {
				t.Fatal(err)
			}
			third, err = controller.own(context.Background(), hustle.ParticipationBlocking, noOpFinalizer)
			if err != nil || third == nil {
				t.Fatalf("third own after finalizer = (%v,%v), want owned", third, err)
			}
			if err := second.finalize(context.Background(), hustle.Outcome{}); err != nil {
				t.Fatal(err)
			}
			if err := third.awaitExecution(); err != nil {
				t.Fatal(err)
			}
			if err := third.finalize(context.Background(), hustle.Outcome{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLanesDoNotBorrowCapacity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		constrained hustle.Participation
		spare       hustle.Participation
	}{
		{name: "background cannot borrow blocking", constrained: hustle.ParticipationBackground, spare: hustle.ParticipationBlocking},
		{name: "blocking cannot borrow background", constrained: hustle.ParticipationBlocking, spare: hustle.ParticipationBackground},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			config.Blocking = LaneLimits{Concurrent: 2, Queued: 1}
			config.Background = LaneLimits{Concurrent: 1, Queued: 1}
			if testCase.constrained == hustle.ParticipationBlocking {
				config.Blocking, config.Background = config.Background, config.Blocking
			}
			controller, err := New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			active, err := controller.own(context.Background(), testCase.constrained, noOpFinalizer)
			if err != nil {
				t.Fatal(err)
			}
			queued, err := controller.own(context.Background(), testCase.constrained, noOpFinalizer)
			if err != nil {
				t.Fatal(err)
			}
			if err := active.awaitExecution(); err != nil {
				t.Fatal(err)
			}
			assertNotGranted(t, queued, "constrained lane borrowed spare execution")
			spare, err := controller.own(context.Background(), testCase.spare, noOpFinalizer)
			if err != nil {
				t.Fatal(err)
			}
			if err := spare.awaitExecution(); err != nil {
				t.Fatalf("spare lane did not grant independently: %v", err)
			}
			if err := active.finalize(context.Background(), hustle.Outcome{}); err != nil {
				t.Fatal(err)
			}
			if err := queued.awaitExecution(); err != nil {
				t.Fatal(err)
			}
			for _, run := range []*ownedRun{queued, spare} {
				if err := run.finalize(context.Background(), hustle.Outcome{}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func assertNotGranted(t *testing.T, run *ownedRun, description string) {
	t.Helper()
	select {
	case <-run.granted:
		t.Fatal(description)
	default:
	}
}

func TestConcurrentCapacityGrantsExactLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		concurrent int
	}{
		{name: "one", concurrent: 1},
		{name: "three", concurrent: 3},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			config.Blocking = LaneLimits{Concurrent: testCase.concurrent, Queued: 1}
			controller, err := New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			runs := make([]*ownedRun, 0, testCase.concurrent+1)
			for index := 0; index < testCase.concurrent+1; index++ {
				run, ownErr := controller.own(context.Background(), hustle.ParticipationBlocking, noOpFinalizer)
				if ownErr != nil {
					t.Fatal(ownErr)
				}
				runs = append(runs, run)
			}
			for index := 0; index < testCase.concurrent; index++ {
				if err := runs[index].awaitExecution(); err != nil {
					t.Fatalf("run[%d] grant error = %v", index, err)
				}
			}
			last := runs[len(runs)-1]
			assertNotGranted(t, last, "queued run granted above concurrent limit")
			if err := runs[0].finalize(context.Background(), hustle.Outcome{}); err != nil {
				t.Fatal(err)
			}
			if err := last.awaitExecution(); err != nil {
				t.Fatal(err)
			}
			var group sync.WaitGroup
			for index := 1; index < len(runs); index++ {
				group.Add(1)
				go func(run *ownedRun) {
					defer group.Done()
					if err := run.finalize(context.Background(), hustle.Outcome{}); err != nil {
						t.Errorf("finalize() error = %v", err)
					}
				}(runs[index])
			}
			group.Wait()
		})
	}
}
