// wasiprobe is the WASI guest the sandboxinit tests run in the embedded
// engine. Build: GOOS=wasip1 GOARCH=wasm go build ./testdata/wasiprobe
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: wasiprobe MODE [ARGS...]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "echo": // args to stdout, one line
		fmt.Println(strings.Join(os.Args[2:], " "))
	case "env": // one variable's value to stdout
		fmt.Println(os.Getenv(os.Args[2]))
	case "environ": // whole environment to stdout
		fmt.Println(strings.Join(os.Environ(), "\n"))
	case "cat": // file (relative to the working directory) to stdout
		b, err := os.ReadFile(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Stdout.Write(b)
	case "write": // write ARGS[3] into file ARGS[2]
		if err := os.WriteFile(os.Args[2], []byte(os.Args[3]), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "stdin": // stdin to stdout, upper-cased
		b, _ := io.ReadAll(os.Stdin)
		os.Stdout.Write([]byte(strings.ToUpper(string(b))))
	case "exit":
		n, _ := strconv.Atoi(os.Args[2])
		os.Exit(n)
	case "spin": // never returns; only a signal or cancel stops it
		for i := 0; ; i++ {
			_ = i
		}
	case "alloc": // grow memory past any sane limit
		var keep [][]byte
		for {
			keep = append(keep, make([]byte, 64<<20))
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown mode", os.Args[1])
		os.Exit(2)
	}
}
