package cli

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

func groupRoot() *cobra.Command {
	root := fakeRoot()
	for _, group := range []string{"secrets", "ci"} {
		g := &cobra.Command{Use: group}
		subs := []string{"list", "push"}
		if group == "ci" {
			subs = []string{"init"}
		}
		for _, s := range subs {
			g.AddCommand(&cobra.Command{Use: s + " [REPO]"})
		}
		root.AddCommand(g)
	}
	return root
}

func TestRewriteArgsCommandGroups(t *testing.T) {
	cases := []struct {
		name     string
		in, want []string
	}{
		{"list with flag", []string{"/repo", "secrets", "list", "--json"}, []string{"secrets", "list", "/repo", "--json"}},
		{"ci init", []string{"/repo", "ci", "init"}, []string{"ci", "init", "/repo"}},
		{"global flags kept", []string{"--target", "prod", "/repo", "secrets", "push", "--yes"}, []string{"secrets", "push", "--target", "prod", "/repo", "--yes"}},
		{"group without sub", []string{"/repo", "secrets"}, []string{"secrets", "/repo"}},
		{"unknown sub is left as rest", []string{"/repo", "secrets", "bogus"}, []string{"secrets", "/repo", "bogus"}},
		{"already ordered", []string{"secrets", "list", "/repo"}, []string{"secrets", "list", "/repo"}},
		{"plain command unchanged", []string{"/repo", "status"}, []string{"status", "/repo"}},
		{"bare", nil, []string{"deploy"}},
		{"repo only", []string{"/repo"}, []string{"deploy", "/repo"}},
		{"help untouched", []string{"/repo", "secrets", "--help"}, []string{"secrets", "/repo", "--help"}},
		{"double dash untouched", []string{"--", "/repo", "secrets", "list"}, []string{"--", "/repo", "secrets", "list"}},
	}
	root := groupRoot()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteArgs(root, tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rewriteArgs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
