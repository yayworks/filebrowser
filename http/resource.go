package http

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/filebrowser/filebrowser/types"
)

const apiResourcePrefix = "/api/resources"

func httpFsErr(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case os.IsPermission(err):
		return http.StatusForbidden
	case os.IsNotExist(err):
		return http.StatusNotFound
	case os.IsExist(err):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func (e *Env) getResourceData(w http.ResponseWriter, r *http.Request, prefix string) (string, *types.User, bool) {
	user, ok := e.getUser(w, r)
	if !ok {
		return "", nil, ok
	}

	path := strings.TrimPrefix(r.URL.Path, prefix)
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		path = "/"
	}

	if !user.IsAllowed(path) {
		httpErr(w, http.StatusForbidden, nil)
		return "", nil, false
	}

	return path, user, true
}

func (e *Env) resourceGetHandler(w http.ResponseWriter, r *http.Request) {
	path, user, ok := e.getResourceData(w, r, apiResourcePrefix)
	if !ok {
		return
	}

	file, err := types.NewFileInfo(user, path)
	if err != nil {
		httpErr(w, httpFsErr(err), err)
		return
	}

	if file.IsDir {
		scope := "/"

		if sort, order, err := handleSortOrder(w, r, scope); err == nil {
			file.Listing.Sort = sort
			file.Listing.Order = order
		} else {
			httpErr(w, http.StatusBadRequest, err)
			return
		}
		file.Listing.ApplySort()
		renderJSON(w, file)
		return
	}

	if file.Type == "video" {
		file.DetectSubtitles()
	}

	if !user.Perm.Modify && file.Type == "text" {
		file.Type = "textImmutable"
	}

	if checksum := r.URL.Query().Get("checksum"); checksum != "" {
		err = file.Checksum(checksum)
		if err == types.ErrInvalidOption {
			httpErr(w, http.StatusBadRequest, nil)
			return
		} else if err != nil {
			httpErr(w, http.StatusInternalServerError, err)
			return
		}

		// do not waste bandwidth if we just want the checksum
		file.Content = ""
	}

	renderJSON(w, file)
}

func (e *Env) resourceDeleteHandler(w http.ResponseWriter, r *http.Request) {
	path, user, ok := e.getResourceData(w, r, apiResourcePrefix)
	if !ok {
		return
	}

	if path == "/" || !user.Perm.Delete {
		httpErr(w, http.StatusForbidden, nil)
		return
	}

	err := e.Runner.Run(func() error {
		return user.Fs.RemoveAll(path)
	}, "delete", path, "", user)

	if err != nil {
		httpErr(w, httpFsErr(err), err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (e *Env) resourcePostPutHandler(w http.ResponseWriter, r *http.Request) {
	path, user, ok := e.getResourceData(w, r, apiResourcePrefix)
	if !ok {
		return
	}

	if !user.Perm.Create && r.Method == http.MethodPost {
		httpErr(w, http.StatusForbidden, nil)
		return
	}

	if !user.Perm.Modify && r.Method == http.MethodPut {
		httpErr(w, http.StatusForbidden, nil)
		return
	}

	defer func() {
		io.Copy(ioutil.Discard, r.Body)
	}()

	// For directories, only allow POST for creation.
	if strings.HasSuffix(r.URL.Path, "/") {
		if r.Method == http.MethodPut {
			httpErr(w, http.StatusMethodNotAllowed, nil)
		} else {
			err := user.Fs.MkdirAll(path, 0775)
			httpErr(w, httpFsErr(err), err)
		}

		return
	}

	if r.Method == http.MethodPost && r.URL.Query().Get("override") != "true" {
		if _, err := user.Fs.Stat(path); err == nil {
			httpErr(w, http.StatusConflict, nil)
			return
		}
	}

	err := e.Runner.Run(func() error {
		file, err := user.Fs.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0775)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(file, r.Body)
		if err != nil {
			return err
		}

		// Gets the info about the file.
		info, err := file.Stat()
		if err != nil {
			return err
		}

		etag := fmt.Sprintf(`"%x%x"`, info.ModTime().UnixNano(), info.Size())
		w.Header().Set("ETag", etag)
		return nil
	}, "upload", path, "", user)

	if err != nil {
		httpErr(w, httpFsErr(err), err)
		return
	}

	httpErr(w, http.StatusOK, nil)
}

func (e *Env) resourcePatchHandler(w http.ResponseWriter, r *http.Request) {
	src, user, ok := e.getResourceData(w, r, apiResourcePrefix)
	if !ok {
		return
	}

	dst := r.URL.Query().Get("destination")
	action := r.URL.Query().Get("action")
	dst, err := url.QueryUnescape(dst)

	if err != nil {
		httpErr(w, httpFsErr(err), err)
		return
	}

	if dst == "/" || src == "/" {
		httpErr(w, http.StatusForbidden, nil)
		return
	}

	switch action {
	case "copy":
		if !user.Perm.Create {
			httpErr(w, http.StatusForbidden, nil)
			return
		}
	case "rename":
	default:
		action = "rename"
		if !user.Perm.Rename {
			httpErr(w, http.StatusForbidden, nil)
			return
		}
	}

	err = e.Runner.Run(func() error {
		if action == "copy" {
			// TODO: err = user.FileSystem.Copy(src, dst)
			return nil
		}

		return user.Fs.Rename(src, dst)
	}, "action", src, dst, user)

	httpErr(w, httpFsErr(err), err)
}

func handleSortOrder(w http.ResponseWriter, r *http.Request, scope string) (sort string, order string, err error) {
	sort = r.URL.Query().Get("sort")
	order = r.URL.Query().Get("order")

	switch sort {
	case "":
		sort = "name"
		if sortCookie, sortErr := r.Cookie("sort"); sortErr == nil {
			sort = sortCookie.Value
		}
	case "name", "size":
		http.SetCookie(w, &http.Cookie{
			Name:   "sort",
			Value:  sort,
			MaxAge: 31536000,
			Path:   scope,
			Secure: r.TLS != nil,
		})
	}

	switch order {
	case "":
		order = "asc"
		if orderCookie, orderErr := r.Cookie("order"); orderErr == nil {
			order = orderCookie.Value
		}
	case "asc", "desc":
		http.SetCookie(w, &http.Cookie{
			Name:   "order",
			Value:  order,
			MaxAge: 31536000,
			Path:   scope,
			Secure: r.TLS != nil,
		})
	}

	return
}