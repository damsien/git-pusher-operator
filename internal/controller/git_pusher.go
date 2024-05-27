package controller

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

	kgiov1 "dams.kgio/kgio/api/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type GitPusher struct {
	resourcesInterceptor kgiov1.ResourcesInterceptor
	interceptedYAML      string
	interceptedGVR       schema.GroupVersionResource
	interceptedName      string
	branch               string
	gitUser              string
	gitEmail             string
	gitToken             string
}

type GitPushResponse struct {
	path       string // The git path were the resource has been pushed
	commitHash string // The commit hash of the commit
}

func (gp *GitPusher) Push() (GitPushResponse, error) {
	gpResponse := &GitPushResponse{path: "", commitHash: ""}
	gp.branch = gp.resourcesInterceptor.Spec.Branch

	// Clone the repository into memory
	repo, err := git.Clone(memory.NewStorage(), memfs.New(), &git.CloneOptions{
		URL:           gp.resourcesInterceptor.Spec.RemoteRepository,
		ReferenceName: plumbing.ReferenceName(gp.branch),
		Auth: &http.BasicAuth{
			Username: gp.gitUser,
			Password: gp.gitToken,
		},
		SingleBranch: true,
	})
	if err != nil {
		errMsg := "failed to clone repository: " + err.Error()
		return *gpResponse, errors.New(errMsg)
	}

	// Get the working directory for the repository
	w, err := repo.Worktree()
	if err != nil {
		errMsg := "failed to get worktree: " + err.Error()
		return *gpResponse, errors.New(errMsg)
	}

	// STEP 1 : Set the path
	path, fileInfo, err := gp.pathConstructor()
	if err != nil {
		return *gpResponse, err
	}

	// STEP 2 : Write the file
	fullFilePath, err := gp.writeFile(path, &fileInfo, w)
	gpResponse.path = fullFilePath
	if err != nil {
		return *gpResponse, err
	}

	// STEP 3 : Commit the changes
	commitHash, err := gp.commitChanges(repo, fullFilePath)
	gpResponse.commitHash = commitHash
	if err != nil {
		return *gpResponse, err
	}

	// STEP 4 : Push the changes
	err = gp.pushChanges(repo)
	if err != nil {
		return *gpResponse, err
	}

	return *gpResponse, nil
}

func (gp *GitPusher) pathConstructor() (string, fs.FileInfo, error) {
	gvr := gp.interceptedGVR
	gvrn := &kgiov1.GroupVersionResourceName{
		GroupVersionResource: &gvr,
	}

	tempPath := kgiov1.GetPathFromGVRN(gp.resourcesInterceptor.Spec.IncludedResources, *gvrn.DeepCopy())

	if tempPath == "" {
		tempPath = gvr.Group + "/" + gvr.Version + "/" + gvr.Resource + "/" + gp.interceptedName + ".yaml"
		tempPath = tempPath[1:]
	}

	path, err := gp.validatePath(tempPath)
	if err != nil {
		return tempPath, nil, err
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			pathDir := path

			// If the end of the path ends with .yaml or .yml
			pathDir, _ = gp.getFileDirName(path, "")

			// Path does not exist, create the directory structure
			err = os.MkdirAll(pathDir, 0755)
			if err != nil {
				return pathDir, nil, err
			}
		} else {
			return tempPath, nil, err
		}
	}

	return path, fileInfo, nil
}

func (gp *GitPusher) validatePath(path string) (string, error) {
	// Validate and clean the path
	cleanPath := filepath.Clean(path)
	// !filepath.IsAbs(cleanPath) test absolute path ?
	if gp.containsInvalidCharacters(cleanPath) {
		return path, errors.New("the path is not valid")
	}

	return cleanPath, nil
}

func (gp *GitPusher) containsInvalidCharacters(path string) bool {
	invalidChars := []rune{':', '*', '?', '"', '<', '>', '|'}
	for _, char := range path {
		for _, invalidChar := range invalidChars {
			if char == invalidChar {
				return true
			}
		}
	}
	return false
}

func (gp *GitPusher) getFileDirName(path string, filename string) (string, string) {
	pathArr := strings.Split(path, "/")
	if filename == "" {
		return path + "/", gp.resourcesInterceptor.Name + ".yaml"
	}
	if strings.Contains(pathArr[len(pathArr)-1], ".yaml") || strings.Contains(pathArr[len(pathArr)-1], ".yml") {
		filename := pathArr[len(pathArr)-1]
		pathArr := pathArr[:len(pathArr)-1]
		return strings.Join(pathArr, "/"), filename
	}
	return strings.Join(pathArr, "/"), gp.resourcesInterceptor.Name + ".yaml"
}

func (gp *GitPusher) writeFile(path string, fileInfo *fs.FileInfo, w *git.Worktree) (string, error) {
	fullFilePath := path
	fInfo := *fileInfo
	dir := ""
	fileName := ""

	if fInfo.IsDir() {
		dir, fileName = gp.getFileDirName(fullFilePath, gp.interceptedName+".yaml")
		fullFilePath = filepath.Join(dir, fileName)
	} else {
		dir, fileName = gp.getFileDirName(path, "")
		fullFilePath = filepath.Join(dir, fileName)
	}
	content := []byte(gp.interceptedYAML)

	file, err := w.Filesystem.Create(fullFilePath)
	if err != nil {
		errMsg := "failed to create file: " + err.Error()
		return fullFilePath, errors.New(errMsg)
	}
	_, err = file.Write(content)
	if err != nil {
		errMsg := "failed to write to file" + err.Error()
		return fullFilePath, errors.New(errMsg)
	}
	file.Close()

	// err := os.WriteFile(fullFilePath, content, 0644)
	// if err != nil {
	// 	errMsg := "failed to create " + fileName + " in the directory " + dir + "; " + err.Error()
	// 	return fullFilePath, errors.New(errMsg)
	// }

	return fullFilePath, nil
}

func (gp *GitPusher) commitChanges(repo *git.Repository, pathToAdd string) (string, error) {
	w, err := repo.Worktree()
	if err != nil {
		errMsg := "failed to get worktree: " + err.Error()
		return "", errors.New(errMsg)
	}

	// Add the file to the staging area
	_, err = w.Add(pathToAdd)
	if err != nil {
		errMsg := "failed to add file to staging area: " + err.Error()
		return "", errors.New(errMsg)
	}

	// Commit the changes
	commit, err := w.Commit("Add or modify "+gp.interceptedGVR.Resource+" "+gp.interceptedName, &git.CommitOptions{
		Author: &object.Signature{
			Name:  gp.gitUser,
			Email: gp.gitEmail,
			When:  time.Now(),
		},
	})
	if err != nil {
		errMsg := "failed to commit changes: " + err.Error()
		return "", errors.New(errMsg)
	}

	return commit.String(), nil
}

func (gp *GitPusher) pushChanges(repo *git.Repository) error {
	err := repo.Push(&git.PushOptions{
		Auth: &http.BasicAuth{
			Username: gp.gitUser,
			Password: gp.gitToken,
		},
	})
	if err != nil {
		errMsg := "failed to push changes: " + err.Error()
		return errors.New(errMsg)
	}

	return nil
}
