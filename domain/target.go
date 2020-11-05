package domain

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Target is a earth target identifier.
type Target struct {
	// Remote and canonical representation.

	// old way
	//Registry    string `json:"registry"`    // github.com
	//ProjectPath string `json:"projectPath"` // earthly/earthly/examples/go
	//Tag         string `json:"tag"`         // main

	// new way
	GitURL  string // "github.com/earthly/earthly"
	GitPath string // "examples/go"
	Tag     string // "main"

	// Local representation.
	LocalPath string `json:"localPath"`

	// Target name.
	Target string `json:"target"`
}

// IsExternal returns whether the target is external to the current project.
func (et Target) IsExternal() bool {
	return et.IsRemote() || et.IsLocalExternal()
}

// IsLocalInternal returns whether the target is a local.
func (et Target) IsLocalInternal() bool {
	return et.LocalPath == "."
}

// IsLocalExternal returns whether the target is a local, but external target.
func (et Target) IsLocalExternal() bool {
	return et.LocalPath != "." && et.LocalPath != ""
}

// IsRemote returns whether the target is remote.
func (et Target) IsRemote() bool {
	return !et.IsLocalExternal() && !et.IsLocalInternal()
}

// String returns a string representation of the Target.
func (et Target) String() string {
	if et.IsLocalExternal() {
		return fmt.Sprintf("%s+%s", et.LocalPath, et.Target)
	}
	if et.IsRemote() {
		tag := fmt.Sprintf(":%s", et.Tag)
		if et.Tag == "" {
			tag = ""
		}
		//fmt.Printf("reg: %s projectPath:%s tag:%s target:%s\n", et.Registry, et.ProjectPath, tag, et.Target)
		return fmt.Sprintf("%s/%s%s+%s", et.GitURL, et.GitPath, tag, et.Target)
	}
	// Local internal.
	return fmt.Sprintf("+%s", et.Target)
}

// StringCanonical returns a string representation of the Target, in canonical form.
func (et Target) StringCanonical() string {
	if et.GitURL != "" {
		tag := fmt.Sprintf(":%s", et.Tag)
		if et.Tag == "" {
			tag = ""
		}
		return fmt.Sprintf("%s/%s%s+%s", et.GitURL, et.GitPath, tag, et.Target)
	}
	return et.String()
}

// ProjectCanonical returns a string representation of the project of the target, in canonical form.
func (et Target) ProjectCanonical() string {
	if et.GitURL != "" {
		tag := fmt.Sprintf(":%s", et.Tag)
		if et.Tag == "" {
			tag = ""
		}
		return fmt.Sprintf("%s/%s%s", et.GitURL, et.GitPath, tag)
	}
	if et.LocalPath == "." {
		return ""
	}
	return path.Base(et.LocalPath)
}

type gitMatcher struct {
	pattern string
	user    string
	suffix  string
}

// returns git path in the form user@host:path/to/repo.git, and any subdir
func parseGitURLandPath(path string) (string, string, error) {
	matchers := []gitMatcher{
		{
			pattern: "github.com/[^/]+/[^/]+",
			user:    "git",
			suffix:  ".git",
		},
		{
			pattern: "gitlab.com/[^/]+/[^/]+",
			user:    "git",
			suffix:  ".git",
		},
		{
			pattern: "bitbucket.com/[^/]+/[^/]+",
			user:    "git",
			suffix:  ".git",
		},
		{
			pattern: "192.168.0.116/my/test/path/[^/]+",
			user:    "alex",
			suffix:  ".git",
		},
	}
	fmt.Println(path)
	for _, m := range matchers {
		r, err := regexp.Compile(m.pattern)
		if err != nil {
			panic(err)
		}
		match := r.FindString(path)
		if match != "" {
			subPath := path[len(match):]
			return match, subPath, nil
		}
		fmt.Println()
	}
	return "", "", nil
}

// ParseTarget parses a string into a Target.
func ParseTarget(fullTargetName string) (Target, error) {
	partsPlus := strings.SplitN(fullTargetName, "+", 2)
	if len(partsPlus) != 2 {
		return Target{}, fmt.Errorf("Invalid target ref %s", fullTargetName)
	}
	if partsPlus[0] == "" {
		// Local target.
		return Target{
			LocalPath: ".",
			Target:    partsPlus[1],
		}, nil
	} else if strings.HasPrefix(partsPlus[0], ".") ||
		strings.HasPrefix(partsPlus[0], "/") {
		// Local external target.
		localPath := partsPlus[0]
		if path.IsAbs(localPath) {
			localPath = path.Clean(localPath)
		} else {
			localPath = path.Clean(localPath)
			if !strings.HasPrefix(localPath, ".") {
				localPath = fmt.Sprintf("./%s", localPath)
			}
		}
		return Target{
			LocalPath: localPath,
			Target:    partsPlus[1],
		}, nil
	} else {
		// Remote target.
		tag := ""
		partsColon := strings.SplitN(partsPlus[0], ":", 2)
		if len(partsColon) == 2 {
			tag = partsColon[1]
		}

		gitURL, gitPath, err := parseGitURLandPath(partsColon[0])
		if err != nil {
			return Target{}, err
		}

		return Target{
			GitURL:  gitURL,
			GitPath: gitPath,
			Tag:     tag,
			Target:  partsPlus[1],
		}, nil
	}
}

// JoinTargets returns the result of interpreting target2 as relative to target1.
func JoinTargets(target1 Target, target2 Target) (Target, error) {
	ret := target2
	if target1.IsRemote() {
		// target1 is remote. Turn relative targets into remote targets.
		if !ret.IsRemote() {
			panic("TODO")
			//ret.Registry = target1.Registry
			//ret.ProjectPath = target1.ProjectPath
			ret.Tag = target1.Tag
			if ret.IsLocalExternal() {
				if path.IsAbs(ret.LocalPath) {
					return Target{}, fmt.Errorf(
						"Absolute path %s not supported as reference in external target context", ret.LocalPath)
				}
				//ret.ProjectPath = path.Join(
				//	target1.ProjectPath, ret.LocalPath)
				ret.LocalPath = ""
			} else if ret.IsLocalInternal() {
				ret.LocalPath = ""
			}
		}
	} else {
		if ret.IsLocalExternal() {
			if path.IsAbs(ret.LocalPath) {
				ret.LocalPath = path.Clean(ret.LocalPath)
			} else {
				ret.LocalPath = path.Join(target1.LocalPath, ret.LocalPath)
				if !strings.HasPrefix(ret.LocalPath, ".") {
					ret.LocalPath = fmt.Sprintf("./%s", ret.LocalPath)
				}
			}
		} else if ret.IsLocalInternal() {
			ret.LocalPath = target1.LocalPath
		}
	}
	return ret, nil
}
