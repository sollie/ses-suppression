package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

type fakeSES struct {
	pages       []*sesv2.ListSuppressedDestinationsOutput
	listInputs  []*sesv2.ListSuppressedDestinationsInput
	deleted      []string
	deleteError  map[string]error
	getError     map[string]error
	stillPresent map[string]bool
	getCalls     []string
}

func (f *fakeSES) ListSuppressedDestinations(_ context.Context, input *sesv2.ListSuppressedDestinationsInput, _ ...func(*sesv2.Options)) (*sesv2.ListSuppressedDestinationsOutput, error) {
	f.listInputs = append(f.listInputs, input)
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

func (f *fakeSES) DeleteSuppressedDestination(_ context.Context, input *sesv2.DeleteSuppressedDestinationInput, _ ...func(*sesv2.Options)) (*sesv2.DeleteSuppressedDestinationOutput, error) {
	address := aws.ToString(input.EmailAddress)
	f.deleted = append(f.deleted, address)
	return &sesv2.DeleteSuppressedDestinationOutput{}, f.deleteError[address]
}

func (f *fakeSES) GetSuppressedDestination(_ context.Context, input *sesv2.GetSuppressedDestinationInput, _ ...func(*sesv2.Options)) (*sesv2.GetSuppressedDestinationOutput, error) {
	address := aws.ToString(input.EmailAddress)
	f.getCalls = append(f.getCalls, address)
	if err, ok := f.getError[address]; ok {
		return nil, err
	}
	if f.stillPresent[address] {
		return &sesv2.GetSuppressedDestinationOutput{SuppressedDestination: &types.SuppressedDestination{EmailAddress: aws.String(address)}}, nil
	}
	return nil, &types.NotFoundException{Message: aws.String("not found")}
}

func TestParseDateRange(t *testing.T) {
	after, before, err := parseDateRange("2026-01-01T00:00:00Z", "2026-06-01T00:00:00Z")
	if err != nil || after == nil || before == nil || !after.Before(*before) {
		t.Fatalf("after=%v before=%v err=%v", after, before, err)
	}

	if after, before, err := parseDateRange("", ""); err != nil || after != nil || before != nil {
		t.Fatalf("expected nil range, after=%v before=%v err=%v", after, before, err)
	}

	if _, _, err := parseDateRange("not-a-date", ""); err == nil {
		t.Fatal("expected error for invalid after timestamp")
	}
	if _, _, err := parseDateRange("", "not-a-date"); err == nil {
		t.Fatal("expected error for invalid before timestamp")
	}
	if _, _, err := parseDateRange("2026-06-01T00:00:00Z", "2026-01-01T00:00:00Z"); err == nil {
		t.Fatal("expected error when after is not earlier than before")
	}
}

func TestSelectReason(t *testing.T) {
	reason, values, err := selectReason("", strings.NewReader("2\n"), io.Discard)
	if err != nil || reason != "complaint" || len(values) != 1 || values[0] != types.SuppressionListReasonComplaint {
		t.Fatalf("got reason=%q values=%v err=%v", reason, values, err)
	}
	if _, _, err := selectReason("other", strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("expected invalid reason error")
	}
}

func TestListDestinationsPaginates(t *testing.T) {
	updated := time.Date(2026, 9, 9, 12, 0, 0, 0, time.FixedZone("offset", 3600))
	fake := &fakeSES{pages: []*sesv2.ListSuppressedDestinationsOutput{
		{SuppressedDestinationSummaries: []types.SuppressedDestinationSummary{{EmailAddress: aws.String("a@example.com"), Reason: types.SuppressionListReasonBounce, LastUpdateTime: &updated}}, NextToken: aws.String("next")},
		{SuppressedDestinationSummaries: []types.SuppressedDestinationSummary{{EmailAddress: aws.String("b@example.com"), Reason: types.SuppressionListReasonBounce}}},
	}}
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	got, err := listDestinations(context.Background(), fake, []types.SuppressionListReason{types.SuppressionListReasonBounce}, &after, &before)
	if err != nil || len(got) != 2 || got[0].LastUpdateTime != "2026-09-09T11:00:00Z" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if len(fake.listInputs) != 2 || aws.ToString(fake.listInputs[1].NextToken) != "next" {
		t.Fatalf("pagination inputs=%+v", fake.listInputs)
	}
	if fake.listInputs[0].StartDate != &after || fake.listInputs[0].EndDate != &before {
		t.Fatalf("date range not forwarded: %+v", fake.listInputs[0])
	}
}

func TestClearDestinations(t *testing.T) {
	items := []destination{{EmailAddress: "a@example.com"}, {EmailAddress: "b@example.com"}}
	fake := &fakeSES{deleteError: map[string]error{"b@example.com": errors.New("denied")}}

	preview, err := clearDestinations(context.Background(), fake, "us-east-1", "bounce", items, true, 0, true, nil)
	if err != nil || !preview.DryRun || len(preview.Destinations) != 2 || len(fake.deleted) != 0 {
		t.Fatalf("preview=%+v deleted=%v err=%v", preview, fake.deleted, err)
	}

	var progress []string
	result, err := clearDestinations(context.Background(), fake, "us-east-1", "bounce", items, false, 0, true, func(item destination, err error) error {
		progress = append(progress, item.EmailAddress)
		return nil
	})
	if err == nil || result.Deleted != 1 || len(result.Failures) != 1 || len(fake.deleted) != 2 {
		t.Fatalf("result=%+v deleted=%v err=%v", result, fake.deleted, err)
	}
	if !result.Verified {
		t.Fatalf("expected Verified=true, result=%+v", result)
	}
	if len(fake.getCalls) != 1 || fake.getCalls[0] != "a@example.com" {
		t.Fatalf("expected verification only for the successfully deleted address, got=%v", fake.getCalls)
	}
	if len(progress) != 2 || progress[0] != "a@example.com" || progress[1] != "b@example.com" {
		t.Fatalf("progress=%v", progress)
	}
}

func TestClearDestinationsFailsWhenStillPresent(t *testing.T) {
	items := []destination{{EmailAddress: "a@example.com"}}
	fake := &fakeSES{stillPresent: map[string]bool{"a@example.com": true}}

	result, err := clearDestinations(context.Background(), fake, "us-east-1", "bounce", items, false, 0, true, nil)
	if err == nil || result.Deleted != 0 || len(result.Failures) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Failures[0].Error != "address still present in suppression list after delete" {
		t.Fatalf("unexpected failure reason: %+v", result.Failures[0])
	}
}

func TestClearDestinationsSkipsVerificationWhenDisabled(t *testing.T) {
	items := []destination{{EmailAddress: "a@example.com"}}
	fake := &fakeSES{stillPresent: map[string]bool{"a@example.com": true}}

	result, err := clearDestinations(context.Background(), fake, "us-east-1", "bounce", items, false, 0, false, nil)
	if err != nil || result.Deleted != 1 || len(fake.getCalls) != 0 || result.Verified {
		t.Fatalf("result=%+v getCalls=%v err=%v", result, fake.getCalls, err)
	}
}

func TestClearDestinationsStopsWhileRateLimited(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeSES{}
	items := []destination{{EmailAddress: "a@example.com"}, {EmailAddress: "b@example.com"}}
	cancel()

	result, err := clearDestinations(ctx, fake, "us-east-1", "all", items, false, time.Hour, true, nil)
	if !errors.Is(err, context.Canceled) || result.Deleted != 1 || len(fake.deleted) != 1 {
		t.Fatalf("result=%+v deleted=%v err=%v", result, fake.deleted, err)
	}
}

func TestClearTableSummaryIsLast(t *testing.T) {
	result := clearResult{
		Region:       "eu-north-1",
		Reason:       "bounce",
		DryRun:       true,
		Matched:      1,
		Destinations: []destination{{EmailAddress: "a@example.com", Reason: "BOUNCE"}},
	}
	var output bytes.Buffer
	if err := render(&output, "table", result); err != nil {
		t.Fatal(err)
	}
	if strings.Index(output.String(), "a@example.com") > strings.Index(output.String(), "REGION") {
		t.Fatalf("summary must follow destinations:\n%s", output.String())
	}
}

func TestRender(t *testing.T) {
	result := listResult{
		Region: "eu-west-1",
		Reason: "bounce",
		Count:  1,
		Destinations: []destination{{
			EmailAddress:   "a@example.com",
			Reason:         "BOUNCE",
			LastUpdateTime: "2026-09-09T11:00:00Z",
		}},
	}

	var table bytes.Buffer
	if err := render(&table, "table", result); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"REGION", "eu-west-1", "a@example.com", "BOUNCE"} {
		if !strings.Contains(table.String(), value) {
			t.Fatalf("table output missing %q:\n%s", value, table.String())
		}
	}

	var output bytes.Buffer
	if err := render(&output, "json", result); err != nil {
		t.Fatal(err)
	}
	var decoded listResult
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil || decoded.Count != 1 {
		t.Fatalf("json=%s decoded=%+v err=%v", output.String(), decoded, err)
	}
}
