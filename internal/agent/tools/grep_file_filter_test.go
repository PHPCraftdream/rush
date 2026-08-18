// File-filtering tests for the grep tool: isTextFile's text-vs-binary
// detection decides which files the search implementations may open.

package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsTextFile(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	tests := []struct {
		name     string
		filename string
		content  []byte
		wantText bool
	}{
		{
			name:     "go file",
			filename: "test.go",
			content:  []byte("package main\n\nfunc main() {}\n"),
			wantText: true,
		},
		{
			name:     "yaml file",
			filename: "config.yaml",
			content:  []byte("key: value\nlist:\n  - item1\n  - item2\n"),
			wantText: true,
		},
		{
			name:     "yml file",
			filename: "config.yml",
			content:  []byte("key: value\n"),
			wantText: true,
		},
		{
			name:     "json file",
			filename: "data.json",
			content:  []byte(`{"key": "value"}`),
			wantText: true,
		},
		{
			name:     "javascript file",
			filename: "script.js",
			content:  []byte("console.log('hello');\n"),
			wantText: true,
		},
		{
			name:     "typescript file",
			filename: "script.ts",
			content:  []byte("const x: string = 'hello';\n"),
			wantText: true,
		},
		{
			name:     "markdown file",
			filename: "README.md",
			content:  []byte("# Title\n\nSome content\n"),
			wantText: true,
		},
		{
			name:     "shell script",
			filename: "script.sh",
			content:  []byte("#!/bin/bash\necho 'hello'\n"),
			wantText: true,
		},
		{
			name:     "python file",
			filename: "script.py",
			content:  []byte("print('hello')\n"),
			wantText: true,
		},
		{
			name:     "xml file",
			filename: "data.xml",
			content:  []byte("<?xml version=\"1.0\"?>\n<root></root>\n"),
			wantText: true,
		},
		{
			name:     "plain text",
			filename: "file.txt",
			content:  []byte("plain text content\n"),
			wantText: true,
		},
		{
			name:     "css file",
			filename: "style.css",
			content:  []byte("body { color: red; }\n"),
			wantText: true,
		},
		{
			name:     "scss file",
			filename: "style.scss",
			content:  []byte("$primary: blue;\nbody { color: $primary; }\n"),
			wantText: true,
		},
		{
			name:     "sass file",
			filename: "style.sass",
			content:  []byte("$primary: blue\nbody\n  color: $primary\n"),
			wantText: true,
		},
		{
			name:     "rust file",
			filename: "main.rs",
			content:  []byte("fn main() {\n    println!(\"Hello, world!\");\n}\n"),
			wantText: true,
		},
		{
			name:     "zig file",
			filename: "main.zig",
			content:  []byte("const std = @import(\"std\");\npub fn main() void {}\n"),
			wantText: true,
		},
		{
			name:     "java file",
			filename: "Main.java",
			content:  []byte("public class Main {\n    public static void main(String[] args) {}\n}\n"),
			wantText: true,
		},
		{
			name:     "c file",
			filename: "main.c",
			content:  []byte("#include <stdio.h>\nint main() { return 0; }\n"),
			wantText: true,
		},
		{
			name:     "cpp file",
			filename: "main.cpp",
			content:  []byte("#include <iostream>\nint main() { return 0; }\n"),
			wantText: true,
		},
		{
			name:     "fish shell",
			filename: "script.fish",
			content:  []byte("#!/usr/bin/env fish\necho 'hello'\n"),
			wantText: true,
		},
		{
			name:     "powershell file",
			filename: "script.ps1",
			content:  []byte("Write-Host 'Hello, World!'\n"),
			wantText: true,
		},
		{
			name:     "cmd batch file",
			filename: "script.bat",
			content:  []byte("@echo off\necho Hello, World!\n"),
			wantText: true,
		},
		{
			name:     "cmd file",
			filename: "script.cmd",
			content:  []byte("@echo off\necho Hello, World!\n"),
			wantText: true,
		},
		{
			name:     "binary exe",
			filename: "binary.exe",
			content:  []byte{0x4D, 0x5A, 0x90, 0x00, 0x03, 0x00, 0x00, 0x00},
			wantText: false,
		},
		{
			name:     "png image",
			filename: "image.png",
			content:  []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
			wantText: false,
		},
		{
			name:     "jpeg image",
			filename: "image.jpg",
			content:  []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46},
			wantText: false,
		},
		{
			name:     "zip archive",
			filename: "archive.zip",
			content:  []byte{0x50, 0x4B, 0x03, 0x04, 0x14, 0x00, 0x00, 0x00},
			wantText: false,
		},
		{
			name:     "pdf file",
			filename: "document.pdf",
			content:  []byte("%PDF-1.4\n%âãÏÓ\n"),
			wantText: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			filePath := filepath.Join(tempDir, tt.filename)
			require.NoError(t, os.WriteFile(filePath, tt.content, 0o644))

			got := isTextFile(filePath)
			require.Equal(t, tt.wantText, got, "isTextFile(%s) = %v, want %v", tt.filename, got, tt.wantText)
		})
	}
}
