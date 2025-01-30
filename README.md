# Email Invoice Manager

Simple Golang CLI tool for scraping your email inbox, finding invoices, extracting their value (either from email body or pdf attachment), organizing and saving them, then sending you a message with an overview.

It is configuration based (see [configuration.json](./configuration.json)), which describes the invoice sources, where to find the price and the google drive destination folder.

Right now, these are the supported platforms:

- Inbox: Gmail (through google cloud API)
- Storage: Google Drive (through google cloud API)
- Messaging: Signal (through callmebot API)

## Running the CLI

Either just do `go run .` or `go build` and use the executable `./email-invoice-manager`.
