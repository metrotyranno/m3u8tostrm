package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
)

//go:embed config.example.json
var exampleConfig string

const examplePlaylist = "http://provider.example:8080/get.php?username=USER&password=PASS&type=m3u_plus&output=ts"

// createConfig writes a first config: asks for the essentials in a terminal, else writes the template.
// It reports whether it ran interactively, so the caller can carry on with setup.
func createConfig(path string) (bool, error) {
	cfg := exampleConfig
	interactive := term.IsTerminal(os.Stdin.Fd())
	if interactive {
		url, out := "", "strm"
		required := func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New("required")
			}
			return nil
		}
		fmt.Println("No config found, creating", path) // not in the form: its Markdown eats Windows backslashes
		err := huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("Playlist URL or file (m3u / m3u_plus)").Value(&url).Validate(required),
			huh.NewInput().Title("Output folder for .strm files").Value(&out).Validate(required),
		)).Run()
		if err != nil {
			return false, err
		}
		cfg = fillConfig(strings.TrimSpace(url), strings.TrimSpace(out))
	}
	// 0600: the playlist URL usually contains the provider username and password.
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		return false, err
	}
	if interactive {
		fmt.Println("Created", path)
	} else {
		fmt.Printf("Created template %s - edit it, then run again.\n", path)
	}
	return interactive, nil
}

// askNext ends first-run setup by asking what to do; the answer is a command for main.
func askNext() (string, error) {
	var next string
	err := huh.NewSelect[string]().Title("Setup done. What next?").Options(
		huh.NewOption("Start syncing now, then every interval (Ctrl+C to stop)", ""),
		huh.NewOption("Sync once and exit", "once"),
		huh.NewOption("Install and start as a background service (asks for admin rights)", "service"),
		huh.NewOption("Exit", "exit"),
	).Value(&next).Run()
	return next, err
}

func fillConfig(url, out string) string {
	return strings.NewReplacer(jsonString(examplePlaylist), jsonString(url), `"strm"`, jsonString(out)).Replace(exampleConfig)
}

// jsonString quotes s without escaping &, < and > so URLs stay readable in the file.
func jsonString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSpace(b.String())
}
