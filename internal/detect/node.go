package detect

import (
	"encoding/json"
)

type packageJSON struct {
	Main    string            `json:"main"`
	Scripts map[string]string `json:"scripts"`
}

func detectNode(r *repoFS, res *Result) bool {
	if !r.isFile("package.json") {
		return false
	}
	res.Kind = Node
	res.Evidence = append(res.Evidence, "package.json")

	var pkg packageJSON
	if text, ok := r.read("package.json"); ok {
		if err := json.Unmarshal([]byte(text), &pkg); err != nil {
			res.warn("package.json could not be parsed (%v); the start command is a placeholder", err)
			pkg = packageJSON{}
		}
	}

	manager, lock := "npm", ""
	switch {
	case r.isFile("pnpm-lock.yaml"):
		manager, lock = "pnpm", "pnpm-lock.yaml"
	case r.isFile("yarn.lock"):
		manager, lock = "yarn", "yarn.lock"
	case r.isFile("package-lock.json"):
		lock = "package-lock.json"
	case r.isFile("npm-shrinkwrap.json"):
		lock = "npm-shrinkwrap.json"
	}
	if lock != "" {
		res.Evidence = append(res.Evidence, lock)
	}
	res.require("node", manager)

	_, hasBuild := pkg.Scripts["build"]
	// A build step usually needs devDependencies, so install everything then.
	prod := !hasBuild
	var install, installFrom string
	switch manager {
	case "pnpm":
		install = "pnpm install --frozen-lockfile"
		if prod {
			install += " --prod"
		}
	case "yarn":
		install = "yarn install --frozen-lockfile"
		if prod {
			install += " --production"
		}
	default:
		switch {
		case lock == "" && prod:
			install = "npm install --omit=dev"
		case lock == "":
			install = "npm install"
		case prod:
			install = "npm ci --omit=dev"
		default:
			install = "npm ci"
		}
	}
	switch {
	case lock == "":
		installFrom = "no lockfile found, so versions are not pinned"
	case hasBuild:
		installFrom = "from " + lock + "; dev dependencies are kept because the build needs them"
	default:
		installFrom = "from " + lock
	}
	res.Setup = append(res.Setup, Value{Value: install, From: installFrom})
	if hasBuild {
		res.Setup = append(res.Setup, Value{Value: manager + " run build", From: `from package.json "build" script`})
	}

	switch {
	case pkg.Scripts["start"] != "":
		res.Command = Value{Value: manager + " start", From: `from package.json "start" script`}
	case pkg.Main != "" && safe(pkg.Main):
		res.Command = Value{Value: "node " + pkg.Main, From: `from package.json "main"`}
	default:
		res.Command = Value{
			Value: manager + " start",
			From:  "assumed",
			Note:  `No "start" script or "main" in package.json: add one, or change this command.`,
		}
		res.warn(`package.json has no "start" script or "main"; the command is a guess`)
	}
	res.setPort(3000, "default for Node apps; make sure your app reads PORT")
	return true
}
