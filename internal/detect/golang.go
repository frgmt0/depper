package detect

import (
	"os"
	"regexp"
	"strings"
)

var goMainPackage = regexp.MustCompile(`(?m)^package main\b`)

func detectGo(r *repoFS, res *Result) bool {
	if !r.isFile("go.mod") {
		return false
	}
	res.Kind = Go
	res.Evidence = append(res.Evidence, "go.mod")
	res.require("go")

	cmds := goCmdDirs(r)
	target, from := "./...", "go.mod"
	switch {
	case len(cmds) == 1:
		target, from = "./cmd/"+cmds[0], "the single main package in cmd/"+cmds[0]
		res.Evidence = append(res.Evidence, "cmd/"+cmds[0])
	case len(cmds) > 1:
		res.Evidence = append(res.Evidence, "cmd/")
		res.warn("cmd/ has several programs (%s); the build step builds ./... , so edit it to build the one you deploy", strings.Join(cmds, ", "))
	case !rootHasMain(r):
		res.warn("no main package found at the repository root or in cmd/; adjust the go build target")
	}
	bin := "bin/" + res.Name
	res.Setup = []Value{{Value: "go build -o " + bin + " " + target, From: "from " + from}}
	res.Command = Value{Value: "./" + bin, From: "the binary built by the setup step"}
	res.Excludes = []Value{{Value: "bin/", From: "built on the server by the setup step"}}
	res.setPort(8080, "default for Go apps; make sure your app listens on it")
	return true
}

// goCmdDirs lists cmd/<x> directories that contain a non-test .go file.
func goCmdDirs(r *repoFS) []string {
	var out []string
	for _, d := range r.subdirs("cmd") {
		if !safe(d) {
			continue
		}
		entries, err := os.ReadDir(r.path("cmd/" + d))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

func rootHasMain(r *repoFS) bool {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		if text, ok := r.read(n); ok && goMainPackage.MatchString(text) {
			return true
		}
	}
	return false
}
