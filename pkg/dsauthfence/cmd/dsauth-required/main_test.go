package main

import (
	"math"
	"testing"
)

func TestValidateRequiredEpoch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		epoch      uint32
		min, max   uint32
		shouldFail bool
	}{
		{name: "baseline", epoch: 1, min: 1, max: 2},
		{name: "target", epoch: 2, min: 1, max: 2},
		{name: "missing-zero", epoch: 0, min: 1, max: 2, shouldFail: true},
		{name: "future", epoch: 3, min: 1, max: 2, shouldFail: true},
		{name: "bad-range", epoch: 1, min: 2, max: 1, shouldFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRequiredEpoch(tc.epoch, tc.min, tc.max)
			if (err != nil) != tc.shouldFail {
				t.Fatalf("err=%v shouldFail=%v", err, tc.shouldFail)
			}
		})
	}
}

func TestSplitNonEmpty(t *testing.T) {
	got := splitNonEmpty(" etcd-a:2379, ,etcd-b:2379 ")
	if len(got) != 2 || got[0] != "etcd-a:2379" || got[1] != "etcd-b:2379" {
		t.Fatalf("got=%v", got)
	}
}

func TestCheckedEpochRangeRejectsUint32Truncation(t *testing.T) {
	if _, _, err := checkedEpochRange(uint64(math.MaxUint32)+1, uint64(math.MaxUint32)+1); err == nil {
		t.Fatal("expected overflow to fail")
	}
	min, max, err := checkedEpochRange(1, 2)
	if err != nil || min != 1 || max != 2 {
		t.Fatalf("min=%d max=%d err=%v", min, max, err)
	}
}
