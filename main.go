package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

type sesAPI interface {
	ListSuppressedDestinations(context.Context, *sesv2.ListSuppressedDestinationsInput, ...func(*sesv2.Options)) (*sesv2.ListSuppressedDestinationsOutput, error)
	DeleteSuppressedDestination(context.Context, *sesv2.DeleteSuppressedDestinationInput, ...func(*sesv2.Options)) (*sesv2.DeleteSuppressedDestinationOutput, error)
	GetSuppressedDestination(context.Context, *sesv2.GetSuppressedDestinationInput, ...func(*sesv2.Options)) (*sesv2.GetSuppressedDestinationOutput, error)
}

type destination struct {
	EmailAddress   string `json:"emailAddress"`
	Reason         string `json:"reason"`
	LastUpdateTime string `json:"lastUpdateTime"`
}

type listResult struct {
	Region       string        `json:"region"`
	Reason       string        `json:"reason"`
	After        string        `json:"after,omitempty"`
	Before       string        `json:"before,omitempty"`
	Count        int           `json:"count"`
	Destinations []destination `json:"destinations"`
}

type clearFailure struct {
	EmailAddress string `json:"emailAddress"`
	Error        string `json:"error"`
}

type clearResult struct {
	Region       string         `json:"region"`
	Reason       string         `json:"reason"`
	After        string         `json:"after,omitempty"`
	Before       string         `json:"before,omitempty"`
	DryRun       bool           `json:"dryRun"`
	Verified     bool           `json:"verified"`
	Matched      int            `json:"matched"`
	Deleted      int            `json:"deleted"`
	Destinations []destination  `json:"destinations,omitempty"`
	Failures     []clearFailure `json:"failures,omitempty"`
}

type commandResult struct {
	format string
	value  any
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	result, err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if result != nil {
		if renderErr := render(os.Stdout, result.format, result.value); renderErr != nil {
			fmt.Fprintln(os.Stderr, renderErr)
			os.Exit(1)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) (*commandResult, error) {
	if len(args) == 0 || (args[0] != "list" && args[0] != "clear") {
		return nil, errors.New("usage: ses-suppression <list|clear> [--reason bounce|complaint|all] [--after RFC3339] [--before RFC3339] [--output table|json] [--delete-interval 1s] [--verify] [--yes]")
	}

	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(errOut)
	reasonFlag := flags.String("reason", "", "bounce, complaint, or all")
	afterFlag := flags.String("after", "", "only include addresses last updated on or after this RFC3339 timestamp")
	beforeFlag := flags.String("before", "", "only include addresses last updated before this RFC3339 timestamp")
	output := flags.String("output", "table", "table or json")
	yes := flags.Bool("yes", false, "perform deletions (clear only)")
	deleteInterval := flags.Duration("delete-interval", time.Second, "minimum delay between deletions")
	verify := flags.Bool("verify", true, "confirm each address is actually removed after deletion (clear only)")
	if err := flags.Parse(args[1:]); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 || (command == "list" && *yes) {
		return nil, errors.New("invalid arguments")
	}
	if *output != "table" && *output != "json" {
		return nil, errors.New("output must be table or json")
	}
	if *deleteInterval < 0 {
		return nil, errors.New("delete-interval must not be negative")
	}

	after, before, err := parseDateRange(*afterFlag, *beforeFlag)
	if err != nil {
		return nil, err
	}

	reason, reasons, err := selectReason(*reasonFlag, in, errOut)
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRetryMaxAttempts(10))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("AWS region is required; set AWS_REGION or configure a profile region")
	}

	client := sesv2.NewFromConfig(cfg)
	destinations, err := listDestinations(ctx, client, reasons, after, before)
	if err != nil {
		return nil, fmt.Errorf("list suppressed destinations: %w", err)
	}
	if command == "list" {
		return &commandResult{format: *output, value: listResult{Region: cfg.Region, Reason: reason, After: formatTime(after), Before: formatTime(before), Count: len(destinations), Destinations: destinations}}, nil
	}
	var progress func(destination, error) error
	if *output == "table" && *yes {
		wroteHeader := false
		progress = func(item destination, err error) error {
			if !wroteHeader {
				if _, writeErr := fmt.Fprintln(out, "STATUS   EMAIL"); writeErr != nil {
					return writeErr
				}
				wroteHeader = true
			}
			if err != nil {
				_, writeErr := fmt.Fprintf(out, "%-7s  %s: %v\n", "FAILED", item.EmailAddress, err)
				return writeErr
			}
			_, writeErr := fmt.Fprintf(out, "%-7s  %s\n", "DELETED", item.EmailAddress)
			return writeErr
		}
	}
	result, err := clearDestinations(ctx, client, cfg.Region, reason, destinations, !*yes, *deleteInterval, *verify, progress)
	result.After = formatTime(after)
	result.Before = formatTime(before)
	return &commandResult{format: *output, value: result}, err
}

func parseDateRange(after, before string) (*time.Time, *time.Time, error) {
	var afterTime, beforeTime *time.Time
	if after != "" {
		parsed, err := time.Parse(time.RFC3339, after)
		if err != nil {
			return nil, nil, fmt.Errorf("after must be an RFC3339 timestamp: %w", err)
		}
		afterTime = &parsed
	}
	if before != "" {
		parsed, err := time.Parse(time.RFC3339, before)
		if err != nil {
			return nil, nil, fmt.Errorf("before must be an RFC3339 timestamp: %w", err)
		}
		beforeTime = &parsed
	}
	if afterTime != nil && beforeTime != nil && !afterTime.Before(*beforeTime) {
		return nil, nil, errors.New("after must be earlier than before")
	}
	return afterTime, beforeTime, nil
}

func render(out io.Writer, format string, value any) error {
	if format == "json" {
		return json.NewEncoder(out).Encode(value)
	}

	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	switch result := value.(type) {
	case listResult:
		if _, err := fmt.Fprintln(table, "REGION\tFILTER\tAFTER\tBEFORE\tCOUNT"); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\n\nEMAIL\tREASON\tLAST UPDATE\n", result.Region, result.Reason, displayOrDash(result.After), displayOrDash(result.Before), result.Count); err != nil {
			return err
		}
		for _, item := range result.Destinations {
			if _, err := fmt.Fprintf(table, "%s\t%s\t%s\n", item.EmailAddress, item.Reason, item.LastUpdateTime); err != nil {
				return err
			}
		}
	case clearResult:
		if len(result.Destinations) != 0 {
			if _, err := fmt.Fprintln(table, "EMAIL\tREASON\tLAST UPDATE"); err != nil {
				return err
			}
			for _, item := range result.Destinations {
				if _, err := fmt.Fprintf(table, "%s\t%s\t%s\n", item.EmailAddress, item.Reason, item.LastUpdateTime); err != nil {
					return err
				}
			}
		}
		if len(result.Destinations) != 0 || result.Matched != 0 {
			if _, err := fmt.Fprintln(table); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(table, "REGION\tFILTER\tAFTER\tBEFORE\tDRY RUN\tVERIFIED\tMATCHED\tDELETED\tFAILED"); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%t\t%t\t%d\t%d\t%d\n", result.Region, result.Reason, displayOrDash(result.After), displayOrDash(result.Before), result.DryRun, result.Verified, result.Matched, result.Deleted, len(result.Failures)); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported result type %T", value)
	}
	return table.Flush()
}

func displayOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func selectReason(value string, in io.Reader, out io.Writer) (string, []types.SuppressionListReason, error) {
	if value == "" {
		if _, err := fmt.Fprintln(out, "Select reason: 1) bounce  2) complaint  3) all"); err != nil {
			return "", nil, fmt.Errorf("write prompt: %w", err)
		}
		if _, err := fmt.Fprint(out, "> "); err != nil {
			return "", nil, fmt.Errorf("write prompt: %w", err)
		}
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", nil, fmt.Errorf("read reason: %w", err)
		}
		value = strings.TrimSpace(line)
		switch value {
		case "1":
			value = "bounce"
		case "2":
			value = "complaint"
		case "3":
			value = "all"
		}
	}

	switch strings.ToLower(value) {
	case "bounce":
		return "bounce", []types.SuppressionListReason{types.SuppressionListReasonBounce}, nil
	case "complaint":
		return "complaint", []types.SuppressionListReason{types.SuppressionListReasonComplaint}, nil
	case "all":
		return "all", nil, nil
	default:
		return "", nil, errors.New("reason must be bounce, complaint, or all")
	}
}

func listDestinations(ctx context.Context, client sesAPI, reasons []types.SuppressionListReason, after, before *time.Time) ([]destination, error) {
	input := &sesv2.ListSuppressedDestinationsInput{Reasons: reasons, PageSize: aws.Int32(1000), StartDate: after, EndDate: before}
	var destinations []destination
	for {
		output, err := client.ListSuppressedDestinations(ctx, input)
		if err != nil {
			return nil, err
		}
		for _, item := range output.SuppressedDestinationSummaries {
			destinations = append(destinations, destination{
				EmailAddress:   aws.ToString(item.EmailAddress),
				Reason:         string(item.Reason),
				LastUpdateTime: formatTime(item.LastUpdateTime),
			})
		}
		if output.NextToken == nil || *output.NextToken == "" {
			return destinations, nil
		}
		input.NextToken = output.NextToken
	}
}

func formatTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func clearDestinations(ctx context.Context, client sesAPI, region, reason string, destinations []destination, dryRun bool, interval time.Duration, verify bool, progress func(destination, error) error) (clearResult, error) {
	result := clearResult{Region: region, Reason: reason, DryRun: dryRun, Verified: verify && !dryRun, Matched: len(destinations)}
	if dryRun {
		result.Destinations = destinations
		return result, nil
	}

	for index, item := range destinations {
		if index != 0 && interval != 0 {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return result, ctx.Err()
			case <-timer.C:
			}
		}
		_, err := client.DeleteSuppressedDestination(ctx, &sesv2.DeleteSuppressedDestinationInput{EmailAddress: aws.String(item.EmailAddress)})
		if err == nil && verify {
			err = verifyRemoved(ctx, client, item.EmailAddress)
		}
		if err != nil {
			result.Failures = append(result.Failures, clearFailure{EmailAddress: item.EmailAddress, Error: err.Error()})
			if progress != nil {
				if progressErr := progress(item, err); progressErr != nil {
					return result, fmt.Errorf("write progress: %w", progressErr)
				}
			}
			continue
		}
		result.Deleted++
		if progress != nil {
			if err := progress(item, nil); err != nil {
				return result, fmt.Errorf("write progress: %w", err)
			}
		}
	}
	if len(result.Failures) != 0 {
		return result, fmt.Errorf("failed to delete %d of %d destinations", len(result.Failures), result.Matched)
	}
	return result, nil
}

// verifyRemoved confirms an address no longer appears in the suppression list
// after a successful delete call, satisfying the "address is actually gone"
// success criteria rather than trusting a nil error from DeleteSuppressedDestination.
func verifyRemoved(ctx context.Context, client sesAPI, emailAddress string) error {
	_, err := client.GetSuppressedDestination(ctx, &sesv2.GetSuppressedDestinationInput{EmailAddress: aws.String(emailAddress)})
	if err == nil {
		return errors.New("address still present in suppression list after delete")
	}
	var notFound *types.NotFoundException
	if errors.As(err, &notFound) {
		return nil
	}
	return fmt.Errorf("verify removal: %w", err)
}
