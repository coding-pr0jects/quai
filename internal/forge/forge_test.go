package forge

import "testing"

func TestEvaluateChecks(t *testing.T) {
	req := []string{"check", "test", "conventions"}

	v := EvaluateChecks([]Check{
		{Name: "check", State: "SUCCESS", Bucket: "pass"},
		{Name: "test", State: "SUCCESS", Bucket: "pass"},
		{Name: "conventions", State: "SUCCESS", Bucket: "pass"},
		{Name: "impact", State: "SKIPPED", Bucket: "skipping"},
	}, req)
	if v.Kind != "green" {
		t.Fatalf("expected green, got %+v", v)
	}

	// Required check skipped must fail.
	vSkip := EvaluateChecks([]Check{
		{Name: "check", State: "SKIPPED", Bucket: "skipping"},
		{Name: "test", State: "SUCCESS", Bucket: "pass"},
		{Name: "conventions", State: "SUCCESS", Bucket: "pass"},
	}, req)
	if vSkip.Kind != "red" || len(vSkip.Failing) != 1 {
		t.Fatalf("expected red on skipped required check, got %+v", vSkip)
	}

	// Non-required check failing must still fail.
	vFail := EvaluateChecks([]Check{
		{Name: "check", State: "SUCCESS", Bucket: "pass"},
		{Name: "test", State: "SUCCESS", Bucket: "pass"},
		{Name: "conventions", State: "SUCCESS", Bucket: "pass"},
		{Name: "formal", State: "FAILURE", Bucket: "fail"},
	}, req)
	if vFail.Kind != "red" || len(vFail.Failing) != 1 {
		t.Fatalf("expected red on failing optional check, got %+v", vFail)
	}

	// Pending required check returns pending.
	vPend := EvaluateChecks([]Check{
		{Name: "check", State: "IN_PROGRESS", Bucket: "pending"},
		{Name: "test", State: "SUCCESS", Bucket: "pass"},
		{Name: "conventions", State: "SUCCESS", Bucket: "pass"},
	}, req)
	if vPend.Kind != "pending" || len(vPend.Pending) != 1 {
		t.Fatalf("expected pending, got %+v", vPend)
	}
}
