package fetcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mholt/archiver"
	"github.com/satori/go.uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fission/fission"
	"github.com/fission/fission/crd"
	storageSvcClient "github.com/fission/fission/storagesvc/client"
)

type (
	FetchRequestType int

	FetchRequest struct {
		FetchType     FetchRequestType  `json:"fetchType"`
		Package       metav1.ObjectMeta `json:"package"`
		Url           string            `json:"url"`
		StorageSvcUrl string            `json:"storagesvcurl"`
		Filename      string            `json:"filename"`
	}

	// UploadRequest send from builder manager describes which
	// deployment package should be upload to storage service.
	UploadRequest struct {
		Filename      string `json:"filename"`
		StorageSvcUrl string `json:"storagesvcurl"`
	}

	// UploadResponse defines the download url of an archive and
	// its checksum.
	UploadResponse struct {
		ArchiveDownloadUrl string           `json:"archiveDownloadUrl"`
		Checksum           fission.Checksum `json:"checksum"`
	}

	Fetcher struct {
		sharedVolumePath string
		fissionClient    *crd.FissionClient
	}
)

const (
	FETCH_SOURCE = iota
	FETCH_DEPLOYMENT
	FETCH_URL // remove this?
)

func MakeFetcher(sharedVolumePath string) *Fetcher {
	fissionClient, _, _, err := crd.MakeFissionClient()
	if err != nil {
		return nil
	}
	return &Fetcher{
		sharedVolumePath: sharedVolumePath,
		fissionClient:    fissionClient,
	}
}

func downloadUrl(url string, localPath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	w, err := os.Create(localPath)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		return err
	}
	err = os.Chmod(localPath, 0600)
	if err != nil {
		return err
	}

	return nil
}

func getChecksum(path string) (*fission.Checksum, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	hasher := sha256.New()
	_, err = io.Copy(hasher, f)
	if err != nil {
		return nil, err
	}

	c := hex.EncodeToString(hasher.Sum(nil))

	return &fission.Checksum{
		Type: fission.ChecksumTypeSHA256,
		Sum:  c,
	}, nil
}

func verifyChecksum(path string, checksum *fission.Checksum) error {
	if checksum.Type != fission.ChecksumTypeSHA256 {
		return fission.MakeError(fission.ErrorInvalidArgument, "Unsupported checksum type")
	}
	c, err := getChecksum(path)
	if err != nil {
		return err
	}
	if c.Sum != checksum.Sum {
		return fission.MakeError(fission.ErrorChecksumFail, "Checksum validation failed")
	}
	return nil
}

func (fetcher *Fetcher) FetchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "only POST is supported on this endpoint", 405)
		return
	}

	startTime := time.Now()
	defer func() {
		elapsed := time.Since(startTime)
		log.Printf("elapsed time in fetch request = %v", elapsed)
	}()

	// parse request
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body")
		http.Error(w, err.Error(), 500)
		return
	}
	var req FetchRequest
	err = json.Unmarshal(body, &req)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		http.Error(w, err.Error(), 400)
		return
	}
	log.Printf("fetcher received fetch request and started downloading: %v", req)

	code, err := fetcher.Fetch(req)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}

	// all done
	w.WriteHeader(http.StatusOK)
}

// Fetch takes FetchRequest and makes the fetch call
// It returns the HTTP code and error if any
func (fetcher *Fetcher) Fetch(req FetchRequest) (int, error) {
	tmpFile := req.Filename + ".tmp"
	tmpPath := filepath.Join(fetcher.sharedVolumePath, tmpFile)

	if req.FetchType == FETCH_URL {
		// fetch the file and save it to the tmp path
		err := downloadUrl(req.Url, tmpPath)
		if err != nil {
			e := fmt.Sprintf("Failed to download url %v: %v", req.Url, err)
			log.Printf(e)
			return 400, errors.New(e)
		}
	} else {
		// get pkg
		pkg, err := fetcher.fissionClient.Packages(req.Package.Namespace).Get(req.Package.Name)
		if err != nil {
			e := fmt.Sprintf("Failed to get package: %v", err)
			log.Printf(e)
			return 500, errors.New(e)
		}

		var archive *fission.Archive
		if req.FetchType == FETCH_SOURCE {
			archive = &pkg.Spec.Source
		} else if req.FetchType == FETCH_DEPLOYMENT {
			archive = &pkg.Spec.Deployment
		}
		// get package data as literal or by url
		if len(archive.Literal) > 0 {
			// write pkg.Literal into tmpPath
			err = ioutil.WriteFile(tmpPath, archive.Literal, 0600)
			if err != nil {
				e := fmt.Sprintf("Failed to write file %v: %v", tmpPath, err)
				log.Printf(e)
				return 500, errors.New(e)
			}
		} else {
			// download and verify
			err = downloadUrl(archive.URL, tmpPath)
			if err != nil {
				e := fmt.Sprintf("Failed to download url %v: %v", req.Url, err)
				log.Printf(e)
				return 400, errors.New(e)
			}

			err = verifyChecksum(tmpPath, &archive.Checksum)
			if err != nil {
				e := fmt.Sprintf("Failed to verify checksum: %v", err)
				log.Printf(e)
				return 400, errors.New(e)
			}
		}
	}

	// check file type here, if the file is a zip file unarchive it.
	if archiver.Zip.Match(tmpPath) {
		// unarchive tmp file to a tmp unarchive path
		tmpUnarchivePath := filepath.Join(fetcher.sharedVolumePath, uuid.NewV4().String())
		err := fetcher.unarchive(tmpPath, tmpUnarchivePath)
		if err != nil {
			log.Println(err.Error())
			return 500, err
		}
		tmpPath = tmpUnarchivePath
	}

	// move tmp file to requested filename
	err := fetcher.rename(tmpPath, filepath.Join(fetcher.sharedVolumePath, req.Filename))
	if err != nil {
		log.Println(err.Error())
		return 500, err
	}
	log.Printf("Successfully placed at %v", filepath.Join(fetcher.sharedVolumePath, req.Filename))
	return 200, nil
}

func (fetcher *Fetcher) UploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "only POST is supported on this endpoint", 405)
		return
	}

	startTime := time.Now()
	defer func() {
		elapsed := time.Since(startTime)
		log.Printf("elapsed time in upload request = %v", elapsed)
	}()

	// parse request
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body")
		http.Error(w, err.Error(), 500)
		return
	}

	var req UploadRequest
	err = json.Unmarshal(body, &req)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		http.Error(w, err.Error(), 400)
		return
	}
	log.Printf("fetcher received upload request: %v", req)

	zipFilename := req.Filename + ".zip"
	srcFilepath := filepath.Join(fetcher.sharedVolumePath, req.Filename)
	dstFilepath := filepath.Join(fetcher.sharedVolumePath, zipFilename)

	err = fetcher.archive(srcFilepath, dstFilepath)
	if err != nil {
		e := fmt.Sprintf("Error archiving zip file: %v", err)
		log.Println(e)
		http.Error(w, e, 500)
		return
	}

	log.Println("Starting upload...")
	ssClient := storageSvcClient.MakeClient(req.StorageSvcUrl)

	fileID, err := ssClient.Upload(dstFilepath, nil)
	if err != nil {
		e := fmt.Sprintf("Error uploading zip file: %v", err)
		log.Println(e)
		http.Error(w, e, 500)
		return
	}

	sum, err := getChecksum(dstFilepath)
	if err != nil {
		e := fmt.Sprintf("Error calculating checksum of zip file: %v", err)
		log.Println(e)
		http.Error(w, e, 500)
		return
	}

	resp := UploadResponse{
		ArchiveDownloadUrl: ssClient.GetUrl(fileID),
		Checksum:           *sum,
	}

	rBody, err := json.Marshal(resp)
	if err != nil {
		e := fmt.Sprintf("Error encoding upload response: %v", err)
		log.Println(e)
		http.Error(w, e, 500)
		return
	}

	log.Println("Completed upload request")
	w.Header().Add("Content-Type", "application/json")
	w.Write(rBody)
	w.WriteHeader(http.StatusOK)
}

func (fetcher *Fetcher) rename(src string, dst string) error {
	err := os.Rename(src, dst)
	if err != nil {
		return errors.New(fmt.Sprintf("Failed to move file: %v", err))
	}
	return nil
}

// archive zips the contents of directory at src into a new zip file
// at dst (note that the contents are zipped, not the directory itself).
func (fetcher *Fetcher) archive(src string, dst string) error {
	var files []string
	target, err := os.Stat(src)
	if err != nil {
		return errors.New(fmt.Sprintf("Failed to zip file: %v", err))
	}
	if target.IsDir() {
		// list all
		fs, _ := ioutil.ReadDir(src)
		for _, f := range fs {
			files = append(files, filepath.Join(src, f.Name()))
		}
	} else {
		files = append(files, src)
	}
	return archiver.Zip.Make(dst, files)
}

// unarchive is a function that unzips a zip file to destination
func (fetcher *Fetcher) unarchive(src string, dst string) error {
	err := archiver.Zip.Open(src, dst)
	if err != nil {
		return errors.New(fmt.Sprintf("Failed to unzip file: %v", err))
	}
	return nil
}
