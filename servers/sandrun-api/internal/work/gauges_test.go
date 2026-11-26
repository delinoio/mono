// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"testing"

	"github.com/jackc/pgx/v5"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

type gaugeRow struct {
	kind, dimension, state int16
	count                  int64
}

type gaugeRows struct {
	pgx.Rows
	data         []gaugeRow
	cursor       int
	scanErr      error
	iterationErr error
	closed       bool
}

func (r *gaugeRows) Next() bool {
	if r.cursor == len(r.data) {
		r.Close()
		return false
	}
	r.cursor++
	return true
}

func (r *gaugeRows) Close()     { r.closed = true }
func (r *gaugeRows) Err() error { return r.iterationErr }

func (r *gaugeRows) Scan(dest ...any) error {
	if r.cursor == 2 && r.scanErr != nil {
		return r.scanErr
	}
	row := r.data[r.cursor-1]
	*dest[0].(*int16), *dest[1].(*int16), *dest[2].(*int16), *dest[3].(*int64) = row.kind, row.dimension, row.state, row.count
	return nil
}

func TestSampleWorkGauges(t *testing.T) {
	kind := int16(sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION)
	normal := int16(sandrunv1.Priority_PRIORITY_NORMAL)
	high := int16(sandrunv1.Priority_PRIORITY_HIGH)
	linux := int16(sandrunv1.GuestOs_GUEST_OS_LINUX)
	windows := int16(sandrunv1.GuestOs_GUEST_OS_WINDOWS)
	request := [3]string{"DEV_SESSION", "NORMAL", "QUEUED"}
	oldRequest := [3]string{"DEV_SESSION", "HIGH", "STARTING"}
	runner := [3]string{"DEV_SESSION", "LINUX", "WARM"}
	oldRunner := [3]string{"DEV_SESSION", "WINDOWS", "BUSY"}
	initial := workGaugeSnapshot{
		requests: gaugeCounts{request: 2, oldRequest: 3},
		runners:  gaugeCounts{runner: 4, oldRunner: 5},
	}
	wantPartial := workGaugeSnapshot{
		requests: gaugeCounts{request: 7, oldRequest: 0},
		runners:  gaugeCounts{runner: 8, oldRunner: 0},
	}
	wantEmpty := workGaugeSnapshot{
		requests: gaugeCounts{request: 0, oldRequest: 0},
		runners:  gaugeCounts{runner: 0, oldRunner: 0},
	}
	requests := []gaugeRow{{kind, normal, int16(reqQueued), 7}, {kind, high, int16(reqQueued), 9}}
	runners := []gaugeRow{{kind, linux, int16(runWarm), 8}, {kind, windows, int16(runWarm), 9}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	failure := errors.New("grouped query fixture error")
	for _, statement := range []string{requestGaugeQuery, runnerGaugeQuery} {
		gauge := "requests"
		if statement == runnerGaugeQuery {
			gauge = "runners"
		}
		for _, phase := range []string{"query", "scan", "iteration"} {
			t.Run(gauge+"/"+phase, func(t *testing.T) {
				var opened []*gaugeRows
				query := func(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
					found := requests
					if sql == runnerGaugeQuery {
						found = runners
					}
					rows := &gaugeRows{data: found[:1]}
					if sql == statement {
						switch phase {
						case "query":
							return nil, failure
						case "scan":
							rows.data = found
							rows.scanErr = failure
						case "iteration":
							rows.data = found[1:]
							rows.iterationErr = failure
						}
					}
					opened = append(opened, rows)
					return rows, nil
				}
				sample := sampleWorkGauges(t.Context(), query, logger, initial)
				want := wantPartial
				if statement == requestGaugeQuery {
					want.requests = nil
				} else {
					want.runners = nil
				}
				assertGaugeSnapshot(t, sample, want)
				for _, rows := range opened {
					if !rows.closed {
						t.Fatal("sample left rows open")
					}
				}
				current := initial.withSample(sample)
				if statement == requestGaugeQuery {
					want.requests = initial.requests
				} else {
					want.runners = initial.runners
				}
				assertGaugeSnapshot(t, current, want)

				complete := sampleWorkGauges(t.Context(), func(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
					if sql == requestGaugeQuery {
						return &gaugeRows{data: requests[:1]}, nil
					}
					return &gaugeRows{data: runners[:1]}, nil
				}, logger, current)
				assertGaugeSnapshot(t, current.withSample(complete), wantPartial)

				empty := sampleWorkGauges(t.Context(), func(context.Context, string, ...any) (pgx.Rows, error) {
					return &gaugeRows{}, nil
				}, logger, complete)
				assertGaugeSnapshot(t, complete.withSample(empty), wantEmpty)
			})
		}
	}
}

func assertGaugeSnapshot(t *testing.T, got, want workGaugeSnapshot) {
	t.Helper()
	if !maps.Equal(got.requests, want.requests) || (got.requests == nil) != (want.requests == nil) ||
		!maps.Equal(got.runners, want.runners) || (got.runners == nil) != (want.runners == nil) {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}
