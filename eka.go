// eka_tantra_extract.go
// Go 1.17. No third-party Go packages.
// Requires the external pdftotext command from poppler-utils.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ExpectedRolloCount         = 39
	MaximumDownloadBytes int64 = 100 * 1024 * 1024
	DownloadTimeout            = 3 * time.Minute
)

var rolloHeadingPattern = regexp.MustCompile(`(?i)^rollo[[:space:]]*(?:n[º°]|№|#)?[[:space:]]*([0-9]{1,2})[[:space:]]*$`)

type Configuration struct {
	SourcePDF            string
	DestinationDirectory string
	SplitRollos          bool
	OperationID          string
	PDFToTextExecutable  string
	SourceIsURL          bool
}

type RolloSection struct {
	Number int
	Body   string
}

type DocumentSections struct {
	Preface       string
	Rollos        []RolloSection
	ThirdPartBody string
}

func main() {
	if err := Run(); err != nil {
		log.Fatalf("ERROR: %v", err)
	}
}

func Run() error {
	configuration := ReadConfiguration()

	if err := ValidateConfiguration(&configuration); err != nil {
		return err
	}

	log.Printf("[PLAN] OperationID: %s", configuration.OperationID)
	log.Printf("[PLAN] SourcePDF: %s", configuration.SourcePDF)
	log.Printf("[PLAN] DestinationDirectory (must not exist): %s", configuration.DestinationDirectory)
	log.Printf("[PLAN] SplitRollos: %t", configuration.SplitRollos)

	destinationParent := filepath.Dir(configuration.DestinationDirectory)
	stagingDirectory, err := os.MkdirTemp(destinationParent, ".eka-tantra-stage-")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(stagingDirectory)

	if err := WriteOperationPlan(stagingDirectory, configuration); err != nil {
		return err
	}

	sourcePDFPath, err := PrepareSourcePDF(configuration, stagingDirectory)
	if err != nil {
		return err
	}
	if err := ValidatePDFSignature(sourcePDFPath); err != nil {
		return err
	}

	originalTextPath := filepath.Join(stagingDirectory, "original.txt")
	if err := ExtractTextFromSourcePDF(
		configuration.PDFToTextExecutable,
		sourcePDFPath,
		originalTextPath,
	); err != nil {
		return err
	}

	originalText, err := os.ReadFile(originalTextPath)
	if err != nil {
		return fmt.Errorf("read extracted text: %w", err)
	}
	if len(bytes.TrimSpace(originalText)) == 0 {
		return errors.New("pdftotext produced empty text; the PDF may contain scanned pages")
	}
	if !utf8.Valid(originalText) {
		return errors.New("pdftotext output is not valid UTF-8")
	}

	cleanedText := NormalizeExtractedText(string(originalText))
	if err := WriteTextFile(filepath.Join(stagingDirectory, "completo.md"), BuildCompleteMarkdown(cleanedText)); err != nil {
		return err
	}

	if configuration.SplitRollos {
		sections, err := SplitEkaTantraSections(cleanedText)
		if err != nil {
			return fmt.Errorf("split Eka Tantra: %w; no destination published", err)
		}
		if err := WriteEkaTantraSections(stagingDirectory, sections); err != nil {
			return err
		}
	}

	// All generated files have been validated and written in a private staging directory.
	// Never overwrite an existing destination, even on a second run.
	if err := ValidateDestinationDoesNotExist(configuration.DestinationDirectory); err != nil {
		return err
	}
	log.Printf("[COMMIT] Publish staged results to %s", configuration.DestinationDirectory)
	if err := os.Rename(stagingDirectory, configuration.DestinationDirectory); err != nil {
		return fmt.Errorf("publish destination directory: %w", err)
	}

	log.Printf("[DONE] Results saved to %s", configuration.DestinationDirectory)
	return nil
}

func ReadConfiguration() Configuration {
	var configuration Configuration
	flag.StringVar(&configuration.SourcePDF, "source", "", "Local PDF path or HTTPS URL")
	flag.StringVar(&configuration.DestinationDirectory, "destination", "", "New output directory (must not exist)")
	flag.BoolVar(&configuration.SplitRollos, "split-rollos", true, "Create 39 Eka Tantra rollo files and the third part")
	flag.Parse()

	configuration.OperationID = fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405Z"), os.Getpid())
	return configuration
}

func ValidateConfiguration(configuration *Configuration) error {
	if strings.TrimSpace(configuration.SourcePDF) == "" {
		return errors.New("missing -source (PDF path or HTTPS URL)")
	}
	if strings.TrimSpace(configuration.DestinationDirectory) == "" {
		return errors.New("missing -destination (new output directory)")
	}
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument: %q", flag.Arg(0))
	}

	configuration.SourceIsURL = strings.HasPrefix(configuration.SourcePDF, "https://") ||
		strings.HasPrefix(configuration.SourcePDF, "http://")

	if configuration.SourceIsURL {
		parsedURL, err := url.Parse(configuration.SourcePDF)
		if err != nil {
			return fmt.Errorf("invalid source URL: %w", err)
		}
		if parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil {
			return errors.New("source URL must use HTTPS, include a host, and contain no credentials")
		}
	} else {
		absoluteSourcePath, err := filepath.Abs(configuration.SourcePDF)
		if err != nil {
			return fmt.Errorf("resolve local source PDF: %w", err)
		}
		configuration.SourcePDF = absoluteSourcePath
		sourceInformation, err := os.Stat(configuration.SourcePDF)
		if err != nil {
			return fmt.Errorf("inspect source PDF: %w", err)
		}
		if !sourceInformation.Mode().IsRegular() {
			return errors.New("source PDF must be a regular file")
		}
	}

	absoluteDestinationPath, err := filepath.Abs(configuration.DestinationDirectory)
	if err != nil {
		return fmt.Errorf("resolve destination directory: %w", err)
	}
	configuration.DestinationDirectory = filepath.Clean(absoluteDestinationPath)

	if err := ValidateDestinationDoesNotExist(configuration.DestinationDirectory); err != nil {
		return err
	}
	parentDirectory := filepath.Dir(configuration.DestinationDirectory)
	parentInformation, err := os.Stat(parentDirectory)
	if err != nil {
		return fmt.Errorf("destination parent directory does not exist: %s: %w", parentDirectory, err)
	}
	if !parentInformation.IsDir() {
		return fmt.Errorf("destination parent is not a directory: %s", parentDirectory)
	}

	configuration.PDFToTextExecutable, err = exec.LookPath("pdftotext")
	if err != nil {
		return errors.New("pdftotext was not found; install it with: sudo apt install poppler-utils")
	}
	return nil
}

func ValidateDestinationDoesNotExist(destinationDirectory string) error {
	_, err := os.Lstat(destinationDirectory)
	if err == nil {
		return fmt.Errorf("destination already exists; refusing to overwrite: %s", destinationDirectory)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("cannot inspect destination %s: %w", destinationDirectory, err)
	}
	return nil
}

func WriteOperationPlan(stagingDirectory string, configuration Configuration) error {
	plan := fmt.Sprintf(
		"OperationID: %s\nSourcePDF: %s\nDestinationDirectory: %s\nSplitRollos: %t\nExternalCommand: %s\nPolicy: never overwrite an existing destination\n",
		configuration.OperationID,
		configuration.SourcePDF,
		configuration.DestinationDirectory,
		configuration.SplitRollos,
		configuration.PDFToTextExecutable,
	)
	return WriteTextFile(filepath.Join(stagingDirectory, "operaciones.log"), plan)
}

func PrepareSourcePDF(configuration Configuration, stagingDirectory string) (string, error) {
	if !configuration.SourceIsURL {
		return configuration.SourcePDF, nil
	}

	destinationPDFPath := filepath.Join(stagingDirectory, "source.pdf")
	log.Printf("[DOWNLOAD] %s -> %s", configuration.SourcePDF, destinationPDFPath)
	if err := DownloadPDFToDestination(configuration.SourcePDF, destinationPDFPath); err != nil {
		return "", err
	}
	return destinationPDFPath, nil
}

func DownloadPDFToDestination(sourceURL string, destinationPDFPath string) error {
	client := &http.Client{
		Timeout: DownloadTimeout,
		CheckRedirect: func(request *http.Request, previousRequests []*http.Request) error {
			if request.URL.Scheme != "https" {
				return errors.New("refusing an HTTP redirect from an HTTPS PDF URL")
			}
			if len(previousRequests) >= 10 {
				return errors.New("too many HTTP redirects")
			}
			return nil
		},
	}

	response, err := client.Get(sourceURL)
	if err != nil {
		return fmt.Errorf("download source PDF: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download source PDF: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > MaximumDownloadBytes {
		return fmt.Errorf("source PDF is larger than the %d-byte download limit", MaximumDownloadBytes)
	}

	destinationFile, err := os.OpenFile(destinationPDFPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create downloaded PDF in staging: %w", err)
	}

	copiedBytes, copyErr := io.Copy(destinationFile, io.LimitReader(response.Body, MaximumDownloadBytes+1))
	closeErr := destinationFile.Close()
	if copyErr != nil {
		return fmt.Errorf("write downloaded PDF: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close downloaded PDF: %w", closeErr)
	}
	if copiedBytes > MaximumDownloadBytes {
		return fmt.Errorf("download exceeded the %d-byte limit", MaximumDownloadBytes)
	}
	return nil
}

func ValidatePDFSignature(sourcePDFPath string) error {
	sourceFile, err := os.Open(sourcePDFPath)
	if err != nil {
		return fmt.Errorf("open source PDF: %w", err)
	}
	defer sourceFile.Close()

	buffer := make([]byte, 1024)
	readBytes, err := sourceFile.Read(buffer)
	if err != nil && err != io.EOF {
		return fmt.Errorf("inspect PDF header: %w", err)
	}
	if !bytes.Contains(buffer[:readBytes], []byte("%PDF-")) {
		return fmt.Errorf("source is not recognized as a PDF (missing %%PDF- header): %s", sourcePDFPath)
	}
	return nil
}

func ExtractTextFromSourcePDF(executable string, sourcePDFPath string, destinationTextPath string) error {
	log.Printf("[EXTRACT] %s -> %s", sourcePDFPath, destinationTextPath)
	command := exec.Command(executable, "-enc", "UTF-8", sourcePDFPath, destinationTextPath)
	commandOutput, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pdftotext failed: %w: %s", err, strings.TrimSpace(string(commandOutput)))
	}
	return nil
}

func NormalizeExtractedText(sourceText string) string {
	text := strings.ReplaceAll(sourceText, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.ReplaceAll(text, "\f", "\n\n")

	var builder strings.Builder
	for _, character := range text {
		if character == '\n' || character == '\t' || character >= 32 {
			if character != '\u00ad' { // Remove a soft hyphen, not an ordinary visible hyphen.
				builder.WriteRune(character)
			}
		}
	}

	lines := strings.Split(builder.String(), "\n")
	for index := 0; index < len(lines); index++ {
		lines[index] = strings.TrimRight(lines[index], " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n")) + "\n"
}

func ParseRolloHeading(line string) (int, bool) {
	matches := rolloHeadingPattern.FindStringSubmatch(strings.TrimSpace(line))
	if len(matches) != 2 {
		return 0, false
	}
	number, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, false
	}
	return number, true
}

func IsThirdPartHeading(line string) bool {
	return strings.EqualFold(strings.TrimSpace(line), "TERCERA PARTE")
}

func BuildCompleteMarkdown(cleanedText string) string {
	var builder strings.Builder
	builder.WriteString("# Eka Tantra\n\n")

	lines := strings.Split(cleanedText, "\n")
	for _, line := range lines {
		if number, found := ParseRolloHeading(line); found {
			fmt.Fprintf(&builder, "\n## Rollo %d\n\n", number)
			continue
		}
		if IsThirdPartHeading(line) {
			builder.WriteString("\n## Tercera parte\n\n")
			continue
		}
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func SplitEkaTantraSections(cleanedText string) (DocumentSections, error) {
	var sections DocumentSections
	var sectionBody strings.Builder
	currentRollo := 0
	inThirdPart := false

	for _, line := range strings.Split(cleanedText, "\n") {
		if !inThirdPart {
			if number, found := ParseRolloHeading(line); found {
				if number != currentRollo+1 {
					return sections, fmt.Errorf("unexpected rollo %d after rollo %d", number, currentRollo)
				}
				if currentRollo == 0 {
					sections.Preface = strings.TrimSpace(sectionBody.String())
				} else {
					sections.Rollos = append(sections.Rollos, RolloSection{
						Number: currentRollo,
						Body:   strings.TrimSpace(sectionBody.String()),
					})
				}
				currentRollo = number
				sectionBody.Reset()
				continue
			}

			if currentRollo == ExpectedRolloCount && IsThirdPartHeading(line) {
				sections.Rollos = append(sections.Rollos, RolloSection{
					Number: currentRollo,
					Body:   strings.TrimSpace(sectionBody.String()),
				})
				sectionBody.Reset()
				inThirdPart = true
				continue
			}
		}
		sectionBody.WriteString(line)
		sectionBody.WriteByte('\n')
	}

	if !inThirdPart {
		return sections, errors.New("third part was not found after rollo 39")
	}
	if len(sections.Rollos) != ExpectedRolloCount {
		return sections, fmt.Errorf("expected %d rollos, found %d", ExpectedRolloCount, len(sections.Rollos))
	}
	sections.ThirdPartBody = strings.TrimSpace(sectionBody.String())
	if sections.ThirdPartBody == "" {
		return sections, errors.New("third part has no text")
	}
	for _, rollo := range sections.Rollos {
		if rollo.Body == "" {
			return sections, fmt.Errorf("rollo %d has no text", rollo.Number)
		}
	}
	return sections, nil
}

func WriteEkaTantraSections(stagingDirectory string, sections DocumentSections) error {
	rollosDirectory := filepath.Join(stagingDirectory, "rollos")
	log.Printf("[WRITE] %d rollos -> %s", len(sections.Rollos), rollosDirectory)
	if err := os.Mkdir(rollosDirectory, 0700); err != nil {
		return fmt.Errorf("create rollos directory: %w", err)
	}

	for _, rollo := range sections.Rollos {
		destinationRolloPath := filepath.Join(rollosDirectory, fmt.Sprintf("rollo-%02d.md", rollo.Number))
		rolloMarkdown := fmt.Sprintf("# Rollo %d\n\n%s\n", rollo.Number, rollo.Body)
		if err := WriteTextFile(destinationRolloPath, rolloMarkdown); err != nil {
			return err
		}
	}

	if err := WriteTextFile(
		filepath.Join(stagingDirectory, "indice-y-prefacio.md"),
		"# Índice y prefacio\n\n"+sections.Preface+"\n",
	); err != nil {
		return err
	}
	if err := WriteTextFile(
		filepath.Join(stagingDirectory, "tercera-parte.md"),
		"# Tercera parte\n\n"+sections.ThirdPartBody+"\n",
	); err != nil {
		return err
	}
	return nil
}

func WriteTextFile(destinationPath string, content string) error {
	file, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create %s: %w", destinationPath, err)
	}
	_, writeErr := io.WriteString(file, content)
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write %s: %w", destinationPath, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", destinationPath, closeErr)
	}
	return nil
}