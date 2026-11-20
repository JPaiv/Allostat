# Contributing

Homeostat is early research code. Issues and pull requests are welcome.

## Before you start

- For anything larger than a small fix, open a proposal issue first.
- This repository is public. Do not put host names, account IDs, inventory
  or incident details from any real system into issues, code or tests.

## Rules for changes

- **In doubt the answer is no.** A change must not turn missing, old or
  unclear evidence into a grant. If a change can turn any refusal into a
  grant, say so in the pull request and explain why it is correct.
- **The decision core stays pure.** The `admission` package has no I/O and
  no clock of its own.
- **New behaviour has a test.**
- Keep comments and documentation short and in simple English.

## Development

```sh
gofmt -l .        # must print nothing
go vet ./...
go test -race ./...
```

## License

By contributing you agree that your contribution is licensed under
Apache-2.0, the license of this repository.
