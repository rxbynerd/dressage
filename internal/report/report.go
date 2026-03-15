// Package report generates a self-contained HTML report from Bedrock log summaries.
package report

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"os"

	"github.com/rubynerd/dressage/internal/model"
)

//go:embed template.html
var templateFS embed.FS

var funcMap = template.FuncMap{
	"formatInt": formatInt,
}

// formatInt formats an int64 with thousands separators.
func formatInt(n int64) string {
	if n < 0 {
		return "-" + formatInt(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// Generate writes the HTML report to the specified file path.
func Generate(report *model.Report, outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer f.Close()

	return Render(report, f)
}

// Render writes the HTML report to the given writer.
func Render(report *model.Report, w io.Writer) error {
	tmplData, err := templateFS.ReadFile("template.html")
	if err != nil {
		return fmt.Errorf("reading embedded template: %w", err)
	}

	tmpl, err := template.New("report").Funcs(funcMap).Parse(string(tmplData))
	if err != nil {
		return fmt.Errorf("parsing template: %w", err)
	}

	if err := tmpl.Execute(w, report); err != nil {
		return fmt.Errorf("executing template: %w", err)
	}

	return nil
}
