package detect

import "fmt"

func detectStatic(r *repoFS, res *Result) bool {
	if !r.isFile("index.html") {
		return false
	}
	res.Kind = Static
	res.Evidence = append(res.Evidence, "index.html")
	res.require("python3")
	res.setPort(8000, "default for python3 -m http.server")
	res.Command = Value{
		Value: fmt.Sprintf("python3 -m http.server %d --bind 127.0.0.1", res.Port),
		From:  "index.html at the repository root",
		Note:  "Serves the files with Python's built-in web server: fine for a simple site, not built for heavy traffic.",
	}
	return true
}
