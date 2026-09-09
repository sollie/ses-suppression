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
	deleted     []string
	deleteError map[string]error
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
	got, err := listDestinations(context.Background(), fake, []types.SuppressionListReason{types.SuppressionListReasonBounce})
	if err != nil || len(got) != 2 || got[0].LastUpdateTime != "2026-09-09T11:00:00Z" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if len(fake.listInputs) != 2 || aws.ToString(fake.listInputs[1].NextToken) != "next" {
		t.Fatalf("pagination inputs=%+v", fake.listInputs)
	}
}

func TestClearDestinations(t *testing.T) {
	items := []destination{{EmailAddress: "a@example.com"}, {EmailAddress: "b@example.com"}}
	fake := &fakeSES{deleteError: map[string]error{"b@example.com": errors.New("denied")}}

	preview, err := clearDestinations(context.Background(), fake, "us-east-1", "bounce", items, true, 0, nil)
	if err != nil || !preview.DryRun || len(preview.Destinations) != 2 || len(fake.deleted) != 0 {
		t.Fatalf("preview=%+v deleted=%v err=%v", preview, fake.deleted, err)
	}

	var progress []string
	result, err := clearDestinations(context.Background(), fake, "us-east-1", "bounce", items, false, 0, func(item destination, err error) error {
		progress = append(progress, item.EmailAddress)
		return nil
	})
	if err == nil || result.Deleted != 1 || len(result.Failures) != 1 || len(fake.deleted) != 2 {
		t.Fatalf("result=%+v deleted=%v err=%v", result, fake.deleted, err)
	}
	if len(progress) != 2 || progress[0] != "a@example.com" || progress[1] != "b@example.com" {
		t.Fatalf("progress=%v", progress)
	}
}

func TestClearDestinationsStopsWhileRateLimited(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeSES{}
	items := []destination{{EmailAddress: "a@example.com"}, {EmailAddress: "b@example.com"}}
	cancel()

	result, err := clearDestinations(ctx, fake, "us-east-1", "all", items, false, time.Hour, nil)
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
