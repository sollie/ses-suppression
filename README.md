# SES suppression list cleaner

Lists and removes addresses from an Amazon SES account-level suppression list. Suppression lists are regional, so run the command once for each region you use.

## Build

Requires Go 1.27.

```sh
go build -o ses-suppression .
```

## Authentication

The tool uses the AWS SDK default credential and region chain. It does not accept or store credentials.

```sh
aws-vault exec my-profile -- ./ses-suppression list --reason bounce
AWS_PROFILE=my-profile AWS_REGION=us-east-1 ./ses-suppression list --reason complaint
```

Environment credentials such as `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `AWS_SESSION_TOKEN` also work. A region must be set through `AWS_REGION` or the selected AWS profile.

## Usage

```sh
./ses-suppression list [--reason bounce|complaint|all] [--output table|json]
./ses-suppression clear [--reason bounce|complaint|all] [--output table|json] [--delete-interval 1s] [--yes]
```

When `--reason` is omitted, the tool prompts for `bounce`, `complaint`, or `all`. Results are tables by default; use `--output json` for JSON. Prompts and errors are written to stderr.

Clearing is a dry run unless `--yes` is passed:

```sh
./ses-suppression clear --reason bounce
./ses-suppression clear --reason bounce --yes
./ses-suppression list --reason all --output json
```

The account-level suppression list supports the `BOUNCE` and `COMPLAINT` reasons. Listing follows all AWS pagination tokens. Deletion is performed sequentially and the command exits nonzero if any address cannot be deleted.

Deletes are spaced one second apart by default to avoid SES API throttling. The AWS SDK also retries transient failures up to 10 times with backoff. Adjust the interval if your account needs a slower or faster rate:

```sh
./ses-suppression clear --reason bounce --yes --delete-interval 2s
```

Table output prints each address as it is deleted or fails, followed by the final totals. JSON output remains a single document emitted after the operation completes.

## IAM

The caller needs `ses:ListSuppressedDestinations`. Actual clearing also needs `ses:DeleteSuppressedDestination`.
