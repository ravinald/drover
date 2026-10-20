// Command herdsay prints a message in a speech bubble above an animal.
//
//	herdsay moo
//	echo "moo" | herdsay
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	msg := strings.Join(os.Args[1:], " ")
	if msg == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herdsay: reading stdin: %v\n", err)
			os.Exit(1)
		}
		msg = string(b)
	}
	if strings.TrimSpace(msg) == "" {
		fmt.Fprintln(os.Stderr, "herdsay: nothing to say: pass a message or pipe one in")
		os.Exit(2)
	}
	out, err := Say(msg, "cow")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdsay: %v\n", err)
		os.Exit(2)
	}
	fmt.Print(out)
}
