package host

import "testing"

func TestParseTableHeader(t *testing.T) {
	for _, tt := range []struct {
		line  string
		key   string
		array bool
		ok    bool
	}{
		{`[deploy]`, "deploy", false, true},
		{`  [deploy]  `, "deploy", false, true},
		{"\t[ deploy ]\r", "", false, false}, // stray CR is not whitespace
		{`[ deploy ]`, "deploy", false, true},
		{`[deploy] # comment`, "deploy", false, true},
		{`[deploy]#c`, "deploy", false, true},
		{`[targets.prod]`, "targets.prod", false, true},
		{`[ targets . "prod" ]`, "targets.prod", false, true},
		{`[targets.'prod']`, "targets.prod", false, true},
		{`[targets."pr.od"]`, "targets.pr.od", false, true},
		{`[deploy."a]b"]`, "deploy.a]b", false, true},
		{`[deploy."a]b"] # c ] d`, "deploy.a]b", false, true},
		{`[deploy.'a]b']`, "deploy.a]b", false, true},
		{`[targets."say \"hi\""]`, `targets.say "hi"`, false, true},
		{`[targets."tab\u0041"]`, "targets.tabA", false, true},
		{`[a-b_C.d9]`, "a-b_C.d9", false, true},
		{`[[targets]]`, "targets", true, true},
		{`[[targets."x]y"]]`, "targets.x]y", true, true},
		{`[[ targets . 'x]]y' ]] # c`, "targets.x]]y", true, true},
		{`[""]`, "", false, true},
		// Not headers.
		{``, "", false, false},
		{`# [deploy]`, "", false, false},
		{`host = "x"`, "", false, false},
		{`[]`, "", false, false},
		{`[deploy`, "", false, false},
		{`[deploy]]`, "", false, false},
		{`[[deploy]`, "", false, false},
		{`[deploy] trailing`, "", false, false},
		{`[deploy."a]`, "", false, false},
		{`[deploy.'a]`, "", false, false},
		{`[deploy..x]`, "", false, false},
		{`[deploy.]`, "", false, false},
		{`[.deploy]`, "", false, false},
		{`[de ploy]`, "", false, false},
		{`["a\q"]`, "", false, false},
		{`["a\u12"]`, "", false, false},
		{`["a\uD800"]`, "", false, false},
		{`["a` + "\x01" + `"]`, "", false, false},
		{`["a"b]`, "", false, false},
		{`["x", "y"]`, "", false, false},
		{`[1, 2]`, "", false, false},
	} {
		key, array, ok := parseTableHeader(tt.line)
		if ok != tt.ok || key != tt.key || array != tt.array {
			t.Errorf("parseTableHeader(%q) = (%q, %v, %v), want (%q, %v, %v)", tt.line, key, array, ok, tt.key, tt.array, tt.ok)
		}
	}
}

func TestEditConfigTextHeadersWithBracketsInQuotedKeys(t *testing.T) {
	values := []configValue{{key: "host", value: `"new.example"`}}
	for _, tt := range []struct {
		name, doc, table, want string
	}{
		{
			"quoted ] in a table before the edited one",
			"[service]\nname = \"a\"\n\n[deploy.\"a]b\"]\nhost = \"other\"\n\n[deploy]\nhost = \"old.example\"\n",
			"deploy",
			"[service]\nname = \"a\"\n\n[deploy.\"a]b\"]\nhost = \"other\"\n\n[deploy]\nhost = \"new.example\"\n",
		},
		{
			"quoted ] in a table after the edited one",
			"[deploy]\nhost = \"old.example\"\n\n[deploy.\"a]b\"]\nhost = \"other\"\n",
			"deploy",
			"[deploy]\nhost = \"new.example\"\n\n[deploy.\"a]b\"]\nhost = \"other\"\n",
		},
		{
			"array table with quoted ] between tables",
			"[deploy]\nhost = \"old.example\"\n\n[[targets.\"x]y\"]]\nhost = \"other\"\n\n[targets.prod]\nuser = \"u\"\n",
			"targets.prod",
			"[deploy]\nhost = \"old.example\"\n\n[[targets.\"x]y\"]]\nhost = \"other\"\n\n[targets.prod]\nhost = \"new.example\"\nuser = \"u\"\n",
		},
		{
			"edited target has a quoted ] sibling before and after",
			"[targets.\"x]y\"]\nhost = \"other\"\n[targets.prod]\nhost = \"old.example\"\n[targets.\"p]q\"]\nhost = \"third\"\n",
			"targets.prod",
			"[targets.\"x]y\"]\nhost = \"other\"\n[targets.prod]\nhost = \"new.example\"\n[targets.\"p]q\"]\nhost = \"third\"\n",
		},
		{
			"edited table named with a quoted ]",
			"[deploy]\nhost = \"a\"\n[targets.\"x]y\"]\nhost = \"old.example\" # keep\n",
			"targets.x]y",
			"[deploy]\nhost = \"a\"\n[targets.\"x]y\"]\nhost = \"new.example\" # keep\n",
		},
		{
			"trailing comment on the edited header",
			"[deploy] # the app host\nuser = \"u\"\n",
			"deploy",
			"[deploy] # the app host\nhost = \"new.example\"\nuser = \"u\"\n",
		},
		{
			"trailing comment on a table that is not edited",
			"[deploy]\nhost = \"old.example\"\n[targets.prod] # staging\nhost = \"other\"\n",
			"deploy",
			"[deploy]\nhost = \"new.example\"\n[targets.prod] # staging\nhost = \"other\"\n",
		},
	} {
		out, _, err := editConfigText(tt.doc, tt.table, values)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if out != tt.want {
			t.Errorf("%s:\n got %q\nwant %q", tt.name, out, tt.want)
		}
	}

	// A missing target next to quoted-bracket tables is appended, not confused
	// with them.
	doc := "[deploy]\nhost = \"a\"\n[targets.\"x]y\"]\nhost = \"other\"\n"
	out, _, err := editConfigText(doc, "targets.prod", values)
	if want := doc + "\n[targets.prod]\nhost = \"new.example\"\n"; err != nil || out != want {
		t.Errorf("append: got %q, err %v, want %q", out, err, want)
	}

	// The same table twice is still detected through a quoted-bracket header.
	if _, _, err := editConfigText("[deploy]\n[deploy.\"a]b\"]\n[ deploy ] # again\n", "deploy", values); err == nil {
		t.Errorf("duplicate [deploy] not reported")
	}
}
