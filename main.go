package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"golang.org/x/net/html"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

type Source struct {
	// Any friendly name for the invoice, like electricity, gas, water, etc.
	BillName string

	// Invoice sender email
	From string

	// Filter invoice emails by subject that contains this string
	SubjectContains string

	// Where the price can be found, either "body" or "attachment"
	Location string

	// What string comes imediately before the price
	StringBeforePrice string

	// What string comes imediately after the price
	StringAfterPrice string
}

type SourceConfig struct {
	// Friendly name for the invoice source group
	Name string

	// Google drive folder ID, you can find it in the url
	DriveDestination string

	// List of invoice sources
	Sources []Source
}

type Invoice struct {
	// Invoice pdf file name with extension
	FileName string

	// Invoice raw pdf file contents
	FileContents []byte

	// Invoice price value in cents
	Value uint64
}

func (i Invoice) String() string {
	return fmt.Sprintf("%s: %d", i.FileName, i.Value)
}

func (i Invoice) Format(f fmt.State, c rune) (int, error) {
	return f.Write([]byte(i.String()))
}

type InvoiceGroup struct {
	// Friendly name for the invoice group
	Name string

	// Google drive folder ID, you can find it in the url
	DriveDestination string

	// List of invoices
	Invoices []Invoice
}

// Retrieve a token, saves the token, then returns the generated client.
func getClient(config *oauth2.Config) *http.Client {
	// The file token.json stores the user's access and refresh tokens, and is
	// created automatically when the authorization flow completes for the first
	// time.
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
	}
	return config.Client(context.Background(), tok)
}

// Request a token from the web, then returns the retrieved token.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		log.Fatalf("Unable to read authorization code: %v", err)
	}

	tok, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		log.Fatalf("Unable to retrieve token from web: %v", err)
	}
	return tok
}

// Retrieves a token from a local file.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

// Saves a token to a file path.
func saveToken(path string, token *oauth2.Token) {
	fmt.Printf("Saving credential file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

// Extracts the content of a pdf page and returns it as a string.
// Uses pdftotext cli tool.
func extractPDFPageContent(source *bytes.Reader, pageNum int) (string, error) {
	// TODO find a good enough library instead of relying in an external cli tool
	// Already tried pdfcpu and it didn't work with all my invoice pdfs unfortunately
	cmd := exec.Command("pdftotext", "-f", strconv.Itoa(pageNum), "-l", strconv.Itoa(pageNum), "-", "-")
	cmd.Stdin = source

	out, err := cmd.Output()

	if err != nil {
		return "", err
	}

	return string(out), nil
}

// Finds and extracts a price value formatted as '%d,%d' in the `haystack`
// by looking for adjacent strings `firstString` and `secondString`.
func extractPriceBetweenTwoStrings(haystack string, firstString string, secondString string) (uint64, error) {
	priceLineIndex := strings.Index(haystack, firstString)

	newLineIndex := strings.Index(haystack[priceLineIndex+len(firstString):], secondString)

	euros := haystack[priceLineIndex+len(firstString) : priceLineIndex+len(firstString)+newLineIndex]

	euros = strings.Trim(euros, " \n\t€abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

	cents := strings.Replace(euros, ",", "", 1)

	centsValue, err := strconv.ParseUint(cents, 10, 16)

	if err != nil {
		return 0, err
	}

	return centsValue, nil
}

// Extracts all the textual content of a html page and returns it as a string
func extractTextFromHtml(input string) string {
	builder := strings.Builder{}
	domDocTest := html.NewTokenizer(strings.NewReader(input))
	previousStartTokenTest := domDocTest.Token()
loopDomTest:
	for {
		tt := domDocTest.Next()
		switch {
		case tt == html.ErrorToken:
			break loopDomTest // End of the document,  done
		case tt == html.StartTagToken:
			previousStartTokenTest = domDocTest.Token()
		case tt == html.TextToken:
			if previousStartTokenTest.Data == "script" || previousStartTokenTest.Data == "style" {
				continue
			}
			TxtContent := strings.TrimSpace(html.UnescapeString(string(domDocTest.Text())))
			if len(TxtContent) > 0 {
				builder.WriteString(TxtContent + "\n")
			}
		}
	}
	return builder.String()
}

// Scrapes the email inbox for invoices and returns them
func scrapeEmailInvoices(client *http.Client, month time.Time, configs []SourceConfig) []InvoiceGroup {
	ctx := context.Background()

	srv, err := gmail.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Unable to retrieve Gmail client: %v", err)
	}

	user := "me"

	nextMonth := month.AddDate(0, 1, 0)

	invoiceGroups := make([]InvoiceGroup, len(configs))

	for configIdx, config := range configs {
		invoiceGroups[configIdx].Name = config.Name
		invoiceGroups[configIdx].DriveDestination = config.DriveDestination
		invoiceGroups[configIdx].Invoices = make([]Invoice, len(config.Sources))
		for sourceIdx, source := range config.Sources {
			query := fmt.Sprintf(
				"after:%d/%d/%d before:%d/%d/%d from:%s",
				month.Year(), month.Month(), month.Day(),
				nextMonth.Year(), nextMonth.Month(), nextMonth.Day(),
				source.From,
			)
			msgs, err := srv.Users.Messages.List(user).Q(query).Do()

			if err != nil {
				log.Fatalf("Unable to retrieve messages: %v", err)
			}
			if len(msgs.Messages) == 0 {
				fmt.Println("No messages found.")
			}

			for _, m := range msgs.Messages {
				msg, err := srv.Users.Messages.Get(user, m.Id).Do()
				if err != nil {
					log.Fatalf("Unable to retrieve message: %v", err)
				}
				internalDate := time.UnixMilli(msg.InternalDate)

				if internalDate.Before(month) || internalDate.After(nextMonth) {
					log.Fatalf("Email is outside of time range")
				}

				// Find subject
				var subjectHeader *gmail.MessagePartHeader
				for _, h := range msg.Payload.Headers {

					if h.Name != "Subject" {
						continue
					}

					if !strings.Contains(h.Value, source.SubjectContains) {
						continue
					}

					subjectHeader = h
					break
				}

				if subjectHeader == nil {
					continue
				}

				fmt.Printf("%s | %v\n", subjectHeader.Value, internalDate)

				// Find attachment
				var attachmentPart *gmail.MessagePart
				var bodyPart *gmail.MessagePart
				for _, part := range msg.Payload.Parts {
					if bodyPart == nil && part.MimeType == "text/html" {
						bodyPart = part
					} else if attachmentPart == nil && part.Filename != "" && part.Body != nil && part.Body.AttachmentId != "" {
						attachmentPart = part
					}
				}

				if attachmentPart == nil {
					fmt.Printf("No attachment found\n")
					continue
				}

				fmt.Printf("Attachment found: %s\n", attachmentPart.Filename)

				attachment, err := srv.Users.Messages.Attachments.Get(
					user, msg.Id, attachmentPart.Body.AttachmentId,
				).Do()

				if err != nil {
					log.Fatalf("Unable to retrieve attachment: %v", err)
				}

				attachmentBytes, err := base64.URLEncoding.DecodeString(attachment.Data)

				if err != nil {
					log.Fatalf("Unable to decode attachment: %v", err)
				}

				var invoiceText string
				switch source.Location {
				case "body":
					if bodyPart == nil {
						log.Fatalf("Unable to find body part")
					}
					decodedBody, err := base64.URLEncoding.DecodeString(bodyPart.Body.Data)

					if err != nil {
						log.Fatalf("Unable to decode body: %v", err)
					}
					decodedBodyString := string(decodedBody)

					invoiceText = extractTextFromHtml(decodedBodyString)
				case "attachment":
					invoiceText, err = extractPDFPageContent(bytes.NewReader(attachmentBytes), 1)

					if err != nil {
						log.Fatalf("Unable to extract page content: %v", err)
					}
				}

				// fmt.Printf("invoiceText: %v\n", invoiceText)

				priceCents, err := extractPriceBetweenTwoStrings(
					invoiceText,
					source.StringBeforePrice,
					source.StringAfterPrice,
				)

				if err != nil {
					log.Fatalf("Unable to extract price: %v", err)
				}

				fmt.Printf("Extracted price (cents): %v\n", priceCents)

				invoiceGroups[configIdx].Invoices[sourceIdx].Value = priceCents
				invoiceGroups[configIdx].Invoices[sourceIdx].FileName = source.BillName + ".pdf"
				invoiceGroups[configIdx].Invoices[sourceIdx].FileContents = attachmentBytes

				break
			}
		}
	}

	return invoiceGroups
}

// Saves invoices to google drive
func saveInvoices(client *http.Client, month time.Time, invoiceGroups []InvoiceGroup) {
	ctx := context.Background()

	driveService, err := drive.NewService(ctx, option.WithHTTPClient(client))

	if err != nil {
		log.Fatalf("Unable to retrieve Drive client: %v", err)
	}

	for _, invoiceGroup := range invoiceGroups {
		var folderMetadata *drive.File = nil
		for invoiceIdx, invoice := range invoiceGroup.Invoices {

			if invoiceIdx == 0 {
				folderMetadata = &drive.File{
					Name:     fmt.Sprintf("%d_%d", month.Year(), month.Month()),
					MimeType: "application/vnd.google-apps.folder",
					Parents:  []string{invoiceGroup.DriveDestination},
				}

				query := fmt.Sprintf(
					`mimeType='application/vnd.google-apps.folder' and
					'%s' in parents and name = '%s' and trashed = false`,
					invoiceGroup.DriveDestination,
					folderMetadata.Name,
				)

				resp, err := driveService.Files.List().
					Q(query).
					Fields("files(id, name)").
					Do()

				if err != nil {
					log.Fatalf("Unable to list files: %v", err)
				}

				if len(resp.Files) > 0 {
					folderMetadata.Id = resp.Files[0].Id
				} else {
					folderMetadata, err = driveService.Files.Create(folderMetadata).Do()

					if err != nil {
						log.Fatalf("Unable to create folder: %v", err)
					}
				}
			}

			if folderMetadata == nil {
				log.Fatalf("unreachable")
			}

			fileMetadata := &drive.File{
				Name: invoice.FileName,
				Parents: []string{
					folderMetadata.Id,
				},
			}

			query := fmt.Sprintf(
				"'%s' in parents and name = '%s' and trashed = false",
				folderMetadata.Id,
				fileMetadata.Name,
			)

			resp, err := driveService.Files.List().
				Q(query).
				Fields("files(id, name)").
				Do()

			if err != nil {
				log.Fatalf("Unable to list files: %v", err)
			}

			if len(resp.Files) > 0 {
				log.Printf("File already exists: %s\n", invoice.FileName)
				continue
			}

			log.Printf("Uploading file: %s\n", invoice.FileName)

			_, err = driveService.Files.Create(fileMetadata).Media(bytes.NewReader(invoice.FileContents)).Do()

			if err != nil {
				log.Fatalf("Unable to create file: %v", err)
			}
		}
	}
}

func readConfiguration() []SourceConfig {
	var configs []SourceConfig

	configBytes, err := os.ReadFile("configuration.json")

	if err != nil {
		log.Fatalf("Unable to read config file: %v", err)
	}

	err = json.Unmarshal(configBytes, &configs)

	if err != nil {
		log.Fatalf("Unable to parse config file: %v", err)
	}

	return configs
}

// Sends invoice summary through Signal
func sendNotification(invoiceGroups []InvoiceGroup, dryRun bool) error {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	phoneNumber := os.Getenv("CALLMEBOT_PHONE_NUMBER")
	if phoneNumber == "" {
		return errors.New("CALLMEBOT_PHONE_NUMBER is not set")
	}
	apiKey := os.Getenv("CALLMEBOT_API_KEY")
	if apiKey == "" {
		return errors.New("CALLMEBOT_API_KEY is not set")
	}

	apiUrl := fmt.Sprintf(
		"https://api.callmebot.com/signal/send.php?phone=%s&apikey=%s&text=",
		phoneNumber,
		apiKey,
	)

	message := strings.Builder{}
	for idx, invoiceGroup := range invoiceGroups {

		if idx > 0 {
			message.WriteString("\n")
		}

		message.WriteString(fmt.Sprintf("%d. %s\n", idx+1, invoiceGroup.Name))
		var total uint64 = 0
		for _, invoice := range invoiceGroup.Invoices {
			total += invoice.Value
			message.WriteString(
				fmt.Sprintf(
					"+ %s - %d,%d\n",
					invoice.FileName,
					invoice.Value/100,
					invoice.Value%100,
				),
			)
		}
		message.WriteString(fmt.Sprintf(
			"Total: %d,%d\n",
			total/100,
			total%100,
		))
	}

	fmt.Printf("Sending notification:\n")

	fmt.Println(message.String())

	if dryRun {
		return nil
	}

	resp, err := http.Get(apiUrl + url.QueryEscape(message.String()))
	if err != nil {
		log.Fatalf("Unable to send notification: %v", err)
	}

	if resp.StatusCode != 200 {
		log.Fatalf("Unable to send notification: %v", resp.Status)
	}

	defer resp.Body.Close()

	return nil
}

func loadAuthenticatedGoogleClient(scope ...string) *http.Client {
	b, err := os.ReadFile("credentials.json")
	if err != nil {
		log.Fatalf("Unable to read client secret file: %v", err)
	}

	// If modifying these scopes, delete your previously saved token.json.
	config, err := google.ConfigFromJSON(b, scope...)
	if err != nil {
		log.Fatalf("Unable to parse client secret file to config: %v", err)
	}
	return getClient(config)
}

func invoiceManager(month time.Time) {
	configs := readConfiguration()
	googleClient := loadAuthenticatedGoogleClient(
		drive.DriveFileScope,
		gmail.GmailReadonlyScope,
	)
	invoiceGroups := scrapeEmailInvoices(googleClient, month, configs)
	fmt.Printf("invoiceGroups: %v\n", invoiceGroups)
	saveInvoices(googleClient, month, invoiceGroups)
	err := sendNotification(invoiceGroups, false)

	if err != nil {
		log.Fatalf("Unable to send notification: %v", err)
	}
}

func main() {
	flag.Parse()

	month := flag.Arg(0)

	if month == "" {
		log.Fatalf("Please provide a month in YYYY-MM format or 'now' for current month")
		return
	}

	var monthTime time.Time
	var err error
	if month == "now" {
		now := time.Now()
		monthTime = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	} else {
		monthTime, err = time.Parse("2006-01", month)
		if err != nil {
			log.Fatalf("Error parsing month: %v", err)
		}
	}
	invoiceManager(monthTime)
}
