package detect

import (
	"sort"

	"github.com/BurntSushi/toml"
)

type cargoManifest struct {
	Package struct {
		Name string `toml:"name"`
	} `toml:"package"`
	Bin []struct {
		Name string `toml:"name"`
	} `toml:"bin"`
	Workspace map[string]any `toml:"workspace"`
}

func detectRust(r *repoFS, res *Result) bool {
	if !r.isFile("Cargo.toml") {
		return false
	}
	res.Kind = Rust
	res.Evidence = append(res.Evidence, "Cargo.toml")
	res.require("cargo")

	var m cargoManifest
	if text, ok := r.read("Cargo.toml"); ok {
		if _, err := toml.Decode(text, &m); err != nil {
			res.warn("Cargo.toml could not be parsed (%v); the binary name is a guess", err)
			m = cargoManifest{}
		}
	}
	var bins []string
	for _, b := range m.Bin {
		if safe(b.Name) {
			bins = append(bins, b.Name)
		}
	}
	sort.Strings(bins)

	bin, from := res.Name, "assumed: same as the service name"
	switch {
	case len(bins) == 1:
		bin, from = bins[0], "from [[bin]] in Cargo.toml"
	case len(bins) > 1:
		bin, from = bins[0], "first [[bin]] in Cargo.toml"
		res.warn("Cargo.toml defines several binaries (%v); the command runs %s", bins, bin)
	case m.Package.Name != "" && safe(m.Package.Name) && r.isFile("src/main.rs"):
		bin, from = m.Package.Name, "from the package name in Cargo.toml"
	case m.Workspace != nil:
		res.warn("Cargo.toml is a workspace root; set the command to the binary you deploy")
	case m.Package.Name != "" && safe(m.Package.Name):
		bin, from = m.Package.Name, "from the package name in Cargo.toml"
	}
	res.Setup = []Value{{Value: "cargo build --release", From: "from Cargo.toml"}}
	res.Command = Value{Value: "./target/release/" + bin, From: from}
	res.Excludes = []Value{{Value: "target/", From: "built on the server by the setup step"}}
	res.setPort(8080, "default for Rust apps; make sure your app listens on it")
	return true
}
