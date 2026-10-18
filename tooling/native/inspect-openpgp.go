//go:build ignore

// Inspect actual Go function tables, including stripped Linux executables.
// Empty/missing tables fail; advisory matching by module name is insufficient.
package main

import (
	"crypto/sha256"
	"debug/elf"
	"debug/gosym"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func inspect(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	section, text := f.Section(".gopclntab"), f.Section(".text")
	if section == nil || text == nil {
		return fmt.Errorf("%s: missing Go function table/text", path)
	}
	pcln, err := section.Data()
	if err != nil {
		return err
	}
	table, err := gosym.NewTable(nil, gosym.NewLineTable(pcln, text.Addr))
	if err != nil {
		return err
	}
	if len(table.Funcs) == 0 {
		return fmt.Errorf("%s: empty Go function table", path)
	}
	for _, function := range table.Funcs {
		if strings.Contains(strings.ToLower(function.Name), "openpgp") {
			return fmt.Errorf("%s: OpenPGP component requires applicability review: %s", path, function.Name)
		}
	}
	binary, err := os.Open(path)
	if err != nil {
		return err
	}
	defer binary.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, binary); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"binary": path, "sha256": hex.EncodeToString(hash.Sum(nil)),
		"functions": len(table.Funcs), "openpgp_functions": 0,
	})
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: inspect-openpgp BINARY...")
		os.Exit(2)
	}
	for _, path := range os.Args[1:] {
		if err := inspect(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
