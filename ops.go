// ops.go — shared write/read operations over a repository, used identically
// by the REST, GraphQL and MCP surfaces: committing a file change through
// the contents API semantics, opening pull requests, analyzing them, and
// merging them (squash here; the true merge lives with the REST surface).

package main

import (
	"sort"
	"strings"
)

// CommitFileInput is one contents-API file mutation.
type CommitFileInput struct {
	Branch  *string // nil: the repository's default branch
	Path    string
	Message string
	Content *[]byte // decoded bytes; nil deletes the file
	Sha     *string // sha the client believes the file has, if supplied
}

// CommitFileResult is what a file mutation produced.
type CommitFileResult struct {
	CommitSha string
	BlobSha   *string
	Created   bool
}

// CommitFileChange applies one file mutation to a branch, creating the
// branch (or the root commit) when needed.
func CommitFileChange(repo *RepoRec, input CommitFileInput) CommitFileResult {
	branch := repo.DefaultBranch
	if input.Branch != nil {
		branch = *input.Branch
	}
	refName := "refs/heads/" + branch
	tip, tipExists := repo.Refs.Get(refName)

	var baseTree *string
	parents := []string{}
	if tipExists {
		commit, ok := repo.Store.Get(tip)
		if !ok || commit.Type != "commit" {
			panic(ApiErrorf(422, "Unprocessable Entity"))
		}
		tree := parseCommit(commit.Data).Tree
		baseTree = &tree
		parents = []string{tip}
	} else if fallback, ok := repo.Refs.Get(headRef(repo)); ok && branch != repo.DefaultBranch {
		if commit, ok := repo.Store.Get(fallback); ok && commit.Type == "commit" {
			tree := parseCommit(commit.Data).Tree
			baseTree = &tree
			parents = []string{fallback}
		}
	}

	flat := make(map[string]string)
	if baseTree != nil {
		flat = flattenTree(repo.Store, *baseTree, "", nil)
	}
	existing, exists := flat[input.Path]
	if input.Content == nil {
		if !exists {
			panic(NotFound())
		}
		if input.Sha != nil && *input.Sha != existing {
			panic(ApiErrorf(409, "Conflict: the file sha does not match"))
		}
		delete(flat, input.Path)
	} else {
		if exists {
			if input.Sha == nil {
				panic(ApiErrorf(422, "Validation Failed: sha is required when updating a file"))
			}
			if *input.Sha != existing {
				panic(ApiErrorf(409, "Conflict: the file sha does not match"))
			}
		} else if input.Sha != nil {
			panic(ApiErrorf(422, "Validation Failed: sha supplied for a file that does not exist"))
		}
		flat[input.Path] = repo.Store.Put("blob", *input.Content)
	}

	tree := buildTreeFromFlat(repo.Store, flat)
	epoch := tickEpochSec()
	commitSha := repo.Store.Put("commit", buildCommitData(tree, parents, input.Message, viewerIdent(epoch)))
	repo.Refs.Set(refName, commitSha)
	repo.UpdatedEpoch = epoch
	var blobSha *string
	if input.Content != nil {
		if oid, ok := flat[input.Path]; ok {
			blobSha = &oid
		}
	}
	return CommitFileResult{
		CommitSha: commitSha,
		BlobSha:   blobSha,
		Created:   !exists && input.Content != nil,
	}
}

// treeOfCommit is the tree oid at a commit, or "" when there is none.
func treeOfCommit(repo *RepoRec, commitSha string) string {
	if commitSha == "" {
		return ""
	}
	commit, ok := repo.Store.Get(commitSha)
	if !ok || commit.Type != "commit" {
		return ""
	}
	return parseCommit(commit.Data).Tree
}

// PullFile is one changed file of a pull request.
type PullFile struct {
	Filename  string
	Status    string // "added" | "removed" | "modified"
	Additions int
	Deletions int
	Changes   int
	Sha       *string
}

// PullAnalysis is the merge base, changed files and conflict flag of a pull.
type PullAnalysis struct {
	Base     *string
	Files    []PullFile
	Conflict bool
}

// AnalyzePull computes the pull's analysis live, exactly like the TS
// pullAnalysis: mergeBase(headTip, baseTip), files from the base->head diff,
// conflict from the three-way merge onto the base tip.
func AnalyzePull(repo *RepoRec, pull *PullRec) PullAnalysis {
	headTip, headOk := repo.Refs.Get("refs/heads/" + pull.HeadRef)
	baseTip, baseOk := repo.Refs.Get("refs/heads/" + pull.BaseRef)
	if !headOk || !baseOk {
		return PullAnalysis{Base: nil, Files: []PullFile{}, Conflict: false}
	}
	base, hasBase := mergeBase(repo.Store, headTip, baseTip)
	if !hasBase {
		return PullAnalysis{Base: nil, Files: []PullFile{}, Conflict: true}
	}
	baseTree := treeOfCommit(repo, base)
	headTree := treeOfCommit(repo, headTip)
	ontoTree := treeOfCommit(repo, baseTip)
	if baseTree == "" || headTree == "" || ontoTree == "" {
		return PullAnalysis{Base: strPtr(base), Files: []PullFile{}, Conflict: false}
	}
	changes := diffTrees(repo.Store, baseTree, headTree)
	paths := make([]string, 0, len(changes))
	for path := range changes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	files := []PullFile{}
	for _, path := range paths {
		change := changes[path]
		additions := 0
		deletions := 0
		if change.To != nil {
			if blob, ok := repo.Store.Get(*change.To); ok {
				additions = countLines(blob.Data)
			}
		}
		if change.From != nil {
			if blob, ok := repo.Store.Get(*change.From); ok {
				deletions = countLines(blob.Data)
			}
		}
		status := "modified"
		if change.From == nil {
			status = "added"
		} else if change.To == nil {
			status = "removed"
		}
		files = append(files, PullFile{
			Filename:  path,
			Status:    status,
			Additions: additions,
			Deletions: deletions,
			Changes:   additions + deletions,
			Sha:       change.To,
		})
	}
	merged := threeWayMerge(repo.Store, baseTree, headTree, ontoTree)
	return PullAnalysis{Base: strPtr(base), Files: files, Conflict: !merged.OK}
}

// OpenPull opens a pull request, validating branches and duplicates.
func OpenPull(repo *RepoRec, title string, body string, head string, base string) *PullRec {
	headBranch := head
	if colon := strings.LastIndex(head, ":"); colon >= 0 {
		headBranch = head[colon+1:]
	}
	if _, ok := repo.Refs.Get("refs/heads/" + headBranch); !ok {
		panic(ApiErrorf(422, "Validation Failed: head branch does not exist"))
	}
	if _, ok := repo.Refs.Get("refs/heads/" + base); !ok {
		panic(ApiErrorf(422, "Validation Failed: base branch does not exist"))
	}
	for _, number := range sortedPullNumbers(repo) {
		existing := repo.Pulls[number]
		if existing.State == "open" && existing.HeadRef == headBranch && existing.BaseRef == base {
			panic(ApiErrorf(422, "Validation Failed: a pull request already exists for %s into %s", headBranch, base))
		}
	}
	epoch := tickEpochSec()
	pull := &PullRec{
		ID:           theForge.AllocObjectID(),
		Number:       repo.NextNumber,
		Title:        title,
		Body:         body,
		State:        "open",
		Merged:       false,
		HeadRef:      headBranch,
		BaseRef:      base,
		CreatedEpoch: epoch,
		UpdatedEpoch: epoch,
	}
	repo.NextNumber++
	repo.Pulls[pull.Number] = pull
	return pull
}

func sortedIssueNumbers(repo *RepoRec) []int {
	numbers := make([]int, 0, len(repo.Issues))
	for number := range repo.Issues {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	return numbers
}

func sortedPullNumbers(repo *RepoRec) []int {
	numbers := make([]int, 0, len(repo.Pulls))
	for number := range repo.Pulls {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	return numbers
}

// SquashMerge squash-merges an open pull request onto its base branch and
// returns the merge commit id.
func SquashMerge(repo *RepoRec, pull *PullRec) string {
	if pull.State != "open" {
		panic(ApiErrorf(422, "Validation Failed: the pull request is not open"))
	}
	headTip, headOk := repo.Refs.Get("refs/heads/" + pull.HeadRef)
	baseTip, baseOk := repo.Refs.Get("refs/heads/" + pull.BaseRef)
	if !headOk || !baseOk {
		panic(ApiErrorf(422, "Validation Failed: a branch is missing"))
	}
	base, hasBase := mergeBase(repo.Store, headTip, baseTip)
	if !hasBase {
		panic(ApiErrorf(405, "Conflict: the branches have no common history"))
	}
	baseTree := treeOfCommit(repo, base)
	headTree := treeOfCommit(repo, headTip)
	ontoTree := treeOfCommit(repo, baseTip)
	merged := threeWayMerge(repo.Store, baseTree, headTree, ontoTree)
	if !merged.OK {
		panic(ApiErrorf(405, "Conflict: the pull request has conflicts"))
	}
	epoch := tickEpochSec()
	message := pull.Title
	if pull.Body != "" {
		message = pull.Title + "\n\n" + pull.Body
	}
	sha := repo.Store.Put("commit", buildCommitData(merged.Tree, []string{baseTip}, message, viewerIdent(epoch)))
	repo.Refs.Set("refs/heads/"+pull.BaseRef, sha)
	pull.State = "closed"
	pull.Merged = true
	mergedEpoch := epoch
	pull.MergedEpoch = &mergedEpoch
	pull.MergeCommitSha = strPtr(sha)
	pull.UpdatedEpoch = epoch
	repo.UpdatedEpoch = epoch
	return sha
}
