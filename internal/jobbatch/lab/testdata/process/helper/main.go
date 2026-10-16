package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "print":
		for i, text := range os.Args[2:] {
			stream := os.Stdout
			if i%2 == 1 {
				stream = os.Stderr
			}
			fmt.Fprint(stream, text)
		}
	case "exit":
		if len(os.Args) != 3 {
			os.Exit(2)
		}
		code, err := strconv.Atoi(os.Args[2])
		if err != nil {
			os.Exit(2)
		}
		os.Exit(code)
	case "hold":
		hold()
	case "spawn":
		program, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		child := platform.Command(context.Background(), program, "hold")
		pipe, err := child.StdoutPipe()
		if err != nil {
			os.Exit(2)
		}
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		line, err := bufio.NewReader(pipe).ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "ready ") {
			_ = child.Process.Kill()
			_ = child.Wait()
			os.Exit(2)
		}
		fmt.Printf("descendant-ready %s", strings.TrimPrefix(line, "ready "))
		hold()
	default:
		os.Exit(2)
	}
}

func hold() {
	fmt.Printf("ready %d\n", os.Getpid())
	// A timer keeps the runtime alive without reading stdin.
	<-time.NewTimer(24 * 365 * time.Hour).C
}
