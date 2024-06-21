/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Commenter provides a way to --query for issues and append a --comment to matches.
//
// The --token determines who interacts with github.
// By default commenter runs in dry mode, add --confirm to make it leave comments.
// The --updated, --include-closed, --ceiling options provide minor safeguards
// around leaving excessive comments.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"sigs.k8s.io/prow/pkg/flagutil"
	"sigs.k8s.io/prow/pkg/github"
)

const (
	templateHelp = `--comment is a golang text/template if set.
	Valid placeholders:
		.Org - github org
		.Repo - github repo
		.Number - issue number
	Advanced (see kubernetes/test-infra/prow/github/types.go):
		.Issue.User.Login - github account
		.Issue.Title
		.Issue.State
		.Issue.HTMLURL
		.Issue.Assignees - list of assigned .Users
		.Issue.Labels - list of applied labels (.Name)
`
)

func flagOptions() options {
	o := options{}

	flag.StringVar(&o.org, "org", "", "GitHub organization (required when using GitHub App credentials)")
	flag.StringVar(&o.query, "query", "", "See https://help.github.com/articles/searching-issues-and-pull-requests/")
	flag.DurationVar(&o.updated, "updated", 2*time.Hour, "Filter to issues unmodified for at least this long if set")
	flag.BoolVar(&o.includeArchived, "include-archived", false, "Match archived issues if set")
	flag.BoolVar(&o.includeClosed, "include-closed", false, "Match closed issues if set")
	flag.BoolVar(&o.includeLocked, "include-locked", false, "Match locked issues if set")
	flag.BoolVar(&o.confirm, "confirm", false, "Mutate github if set")
	flag.StringVar(&o.comment, "comment", "", "Append the following comment to matching issues")
	flag.BoolVar(&o.useTemplate, "template", false, templateHelp)
	flag.IntVar(&o.ceiling, "ceiling", 3, "Maximum number of issues to modify, 0 for infinite")
	flag.BoolVar(&o.random, "random", false, "Choose random issues to comment on from the query")

	o.github.AddFlags(flag.CommandLine)

	flag.Parse()
	return o
}

type meta struct {
	Number int
	Org    string
	Repo   string
	Issue  github.Issue
}

type options struct {
	ceiling         int
	comment         string
	org             string
	includeArchived bool
	includeClosed   bool
	includeLocked   bool
	useTemplate     bool
	query           string
	updated         time.Duration
	confirm         bool
	random          bool
	github          flagutil.GitHubOptions
}

func parseHTMLURL(url string) (string, string, int, error) {
	// Example: https://github.com/batterseapower/pinyin-toolkit/issues/132
	re := regexp.MustCompile(`.+/(.+)/(.+)/(issues|pull)/(\d+)$`)
	mat := re.FindStringSubmatch(url)
	if mat == nil {
		return "", "", 0, fmt.Errorf("failed to parse: %s", url)
	}
	n, err := strconv.Atoi(mat[4])
	if err != nil {
		return "", "", 0, err
	}
	return mat[1], mat[2], n, nil
}

func makeQuery(query string, includeArchived, includeClosed, includeLocked bool, minUpdated time.Duration) (string, error) {
	// GitHub used to allow \n but changed it at some point to result in no results at all
	query = strings.ReplaceAll(query, "\n", " ")
	parts := []string{query}
	if !includeArchived {
		if strings.Contains(query, "archived:true") {
			return "", errors.New("archived:true requires --include-archived")
		}
		parts = append(parts, "archived:false")
	} else if strings.Contains(query, "archived:false") {
		return "", errors.New("archived:false conflicts with --include-archived")
	}
	if !includeClosed {
		if strings.Contains(query, "is:closed") {
			return "", errors.New("is:closed requires --include-closed")
		}
		parts = append(parts, "is:open")
	} else if strings.Contains(query, "is:open") {
		return "", errors.New("is:open conflicts with --include-closed")
	}
	if !includeLocked {
		if strings.Contains(query, "is:locked") {
			return "", errors.New("is:locked requires --include-locked")
		}
		parts = append(parts, "is:unlocked")
	} else if strings.Contains(query, "is:unlocked") {
		return "", errors.New("is:unlocked conflicts with --include-locked")
	}
	if minUpdated != 0 {
		latest := time.Now().Add(-minUpdated)
		parts = append(parts, "updated:<="+latest.Format(time.RFC3339))
	}
	return strings.Join(parts, " "), nil
}

type client interface {
	CreateComment(org, repo string, number int, comment string) error
	FindIssuesWithOrg(org, query, sort string, asc bool) ([]github.Issue, error)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	o := flagOptions()

	if o.query == "" {
		log.Fatal("empty --query")
	}
	if o.github.TokenPath == "" && o.github.AppID == "" {
		log.Fatal("no github authentication options specified")
	}
	if o.github.AppID != "" && o.org == "" {
		log.Fatal("using github appid requires using --org flag")
	}
	if o.comment == "" {
		log.Fatal("empty --comment")
	}

	githubOptsErr := o.github.Validate(true)
	if githubOptsErr != nil {
		log.Fatalf("Error validating github options: %v", githubOptsErr)
	}

	c, err := o.github.GitHubClient(!o.confirm)
	if err != nil {
		log.Fatalf("Failed to construct GitHub client: %v", err)
	}

	query, err := makeQuery(o.query, o.includeArchived, o.includeClosed, o.includeLocked, o.updated)
	if err != nil {
		log.Fatalf("Bad query %q: %v", o.query, err)
	}
	sort := ""
	asc := false
	if o.updated > 0 {
		sort = "updated"
		asc = true
	}
	commenter := makeCommenter(o.comment, o.useTemplate)
	if err := run(c, o.org, query, sort, asc, o.random, commenter, o.ceiling); err != nil {
		log.Fatalf("Failed run: %v", err)
	}
}

func makeCommenter(comment string, useTemplate bool) func(meta) (string, error) {
	if !useTemplate {
		return func(_ meta) (string, error) {
			return comment, nil
		}
	}
	t := template.Must(template.New("comment").Parse(comment))
	return func(m meta) (string, error) {
		out := bytes.Buffer{}
		err := t.Execute(&out, m)
		return out.String(), err
	}
}

func run(c client, org, query, sort string, asc, random bool, commenter func(meta) (string, error), ceiling int) error {
	log.Printf("Searching: %s", query)
	issues, err := c.FindIssuesWithOrg(org, query, sort, asc)
	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}
	problems := []string{}
	log.Printf("Found %d matches", len(issues))
	if random {
		rand.Shuffle(len(issues), func(i, j int) {
			issues[i], issues[j] = issues[j], issues[i]
		})

	}
	for n, i := range issues {
		if ceiling > 0 && n == ceiling {
			log.Printf("Stopping at --ceiling=%d of %d results", n, len(issues))
			break
		}
		log.Printf("Matched %s (%s)", i.HTMLURL, i.Title)
		org, repo, number, err := parseHTMLURL(i.HTMLURL)
		if err != nil {
			msg := fmt.Sprintf("Failed to parse %s: %v", i.HTMLURL, err)
			log.Print(msg)
			problems = append(problems, msg)
		}
		comment, err := commenter(meta{Number: number, Org: org, Repo: repo, Issue: i})
		if err != nil {
			msg := fmt.Sprintf("Failed to create comment for %s/%s#%d: %v", org, repo, number, err)
			log.Print(msg)
			problems = append(problems, msg)
			continue
		}
		if err := c.CreateComment(org, repo, number, comment); err != nil {
			msg := fmt.Sprintf("Failed to apply comment to %s/%s#%d: %v", org, repo, number, err)
			log.Print(msg)
			problems = append(problems, msg)
			continue
		}
		log.Printf("Commented on %s", i.HTMLURL)
	}
	if len(problems) > 0 {
		return fmt.Errorf("encoutered %d failures: %v", len(problems), problems)
	}
	return nil
}
