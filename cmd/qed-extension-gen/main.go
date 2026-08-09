// Command qed-extension-gen generates a standalone self-exec Extension catalog
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/qed-runtime/qed/extension/selfexec"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	flags := flag.NewFlagSet("qed-extension-gen", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	lockPath := flags.String("lock", selfexec.LockFilename, "Extension lock file")
	outputPath := flags.String("output", "registry_gen.go", "generated Go catalog file")
	packageName := flags.String("package", selfexec.DefaultGeneratedPackage, "generated Go package name")
	variableName := flags.String("variable", selfexec.DefaultGeneratedVariable, "exported Catalog variable name")
	check := flags.Bool("check", false, "verify generated source without writing")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(os.Stderr, "qed-extension-gen does not accept positional arguments")
		return 2
	}
	source, err := selfexec.Generate(*lockPath, selfexec.GenerateOptions{
		PackageName:  *packageName,
		VariableName: *variableName,
	})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "generate Extension catalog: %v\n", err)
		return 1
	}
	if *check {
		current, err := selfexec.CheckGenerated(*outputPath, source)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "check Extension catalog: %v\n", err)
			return 1
		}
		if !current {
			_, _ = fmt.Fprintf(os.Stderr, "generated Extension catalog %q is stale\n", *outputPath)
			return 1
		}
		_, _ = fmt.Fprintf(os.Stdout, "Extension catalog %s is current\n", *outputPath)
		return 0
	}
	if err := selfexec.WriteGenerated(*outputPath, source); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "write Extension catalog: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(os.Stdout, "Generated Extension catalog %s from %s\n", *outputPath, *lockPath)
	return 0
}
