package ccv2

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"path/filepath"

	"code.cloudfoundry.org/cli/api/cloudcontroller"
	"code.cloudfoundry.org/cli/api/cloudcontroller/ccerror"
	"code.cloudfoundry.org/cli/api/cloudcontroller/ccv2/internal"
)

// Buildpack represents a Cloud Controller Buildpack.
type Buildpack struct {
	Enabled  bool   `json:"enabled"`
	GUID     string `json:"guid,omitempty"`
	Name     string `json:"name"`
	Position int    `json:"position,omitempty"`
}

func (buildpack *Buildpack) UnmarshalJSON(data []byte) error {
	var alias struct {
		Metadata struct {
			GUID string `json:"guid"`
		} `json:"metadata"`
		Entity struct {
			Name     string `json:"name"`
			Position int    `json:"position"`
			Enabled  bool   `json:"enabled"`
		} `json:"entity"`
	}
	err := json.Unmarshal(data, &alias)
	if err != nil {
		return err
	}

	buildpack.Enabled = alias.Entity.Enabled
	buildpack.GUID = alias.Metadata.GUID
	buildpack.Name = alias.Entity.Name
	buildpack.Position = alias.Entity.Position

	return nil
}

// CreateBuildpack creates a new buildpack.
func (client *Client) CreateBuildpack(buildpack Buildpack) (Buildpack, Warnings, error) {
	body, err := json.Marshal(buildpack)
	if err != nil {
		return Buildpack{}, nil, err
	}

	request, err := client.newHTTPRequest(requestOptions{
		RequestName: internal.PostBuildpackRequest,
		Body:        bytes.NewReader(body),
	})
	if err != nil {
		return Buildpack{}, nil, err
	}
	var createdBuildpack Buildpack
	response := cloudcontroller.Response{
		Result: &createdBuildpack,
	}

	err = client.connection.Make(request, &response)
	return createdBuildpack, response.Warnings, err
}

// GetBuildpacks searches for a buildpack with the given name and returns it if it exists.
func (client *Client) GetBuildpacks(filters ...Filter) ([]Buildpack, Warnings, error) {
	request, err := client.newHTTPRequest(requestOptions{
		RequestName: internal.GetBuildpacksRequest,
		Query:       ConvertFilterParameters(filters),
	})

	if err != nil {
		return nil, nil, err
	}

	var buildpacks []Buildpack
	warnings, err := client.paginate(request, Buildpack{}, func(item interface{}) error {
		if buildpack, ok := item.(Buildpack); ok {
			buildpacks = append(buildpacks, buildpack)
		} else {
			return ccerror.UnknownObjectInListError{
				Expected:   Buildpack{},
				Unexpected: item,
			}
		}
		return nil
	})

	return buildpacks, warnings, err
}

// UpdateBuildpack updates the buildpack with the provided GUID and returns the updated buildpack.
func (client *Client) UpdateBuildpack(buildpack Buildpack) (Buildpack, Warnings, error) {
	body, err := json.Marshal(buildpack)
	if err != nil {
		return Buildpack{}, nil, err
	}

	request, err := client.newHTTPRequest(requestOptions{
		RequestName: internal.PutBuildpackRequest,
		URIParams:   Params{"buildpack_guid": buildpack.GUID},
		Body:        bytes.NewReader(body),
	})
	if err != nil {
		return Buildpack{}, nil, err
	}

	var updatedBuildpack Buildpack
	response := cloudcontroller.Response{
		Result: &updatedBuildpack,
	}

	err = client.connection.Make(request, &response)
	if err != nil {
		return Buildpack{}, response.Warnings, err
	}

	return updatedBuildpack, response.Warnings, nil
}

// UploadBuildpack uploads the contents of a buildpack zip to the server.
func (client *Client) UploadBuildpack(buildpackGUID string, buildpackPath string, buildpack io.Reader, buildpackLength int64) (Warnings, error) {

	contentLength, err := client.calculateBuildpackRequestSize(buildpackLength, buildpackPath)
	if err != nil {
		return nil, err
	}

	contentType, body, writeErrors := client.createMultipartBodyAndHeaderForBuildpack(buildpack, buildpackPath)

	request, err := client.newHTTPRequest(requestOptions{
		RequestName: internal.PutBuildpackBitsRequest,
		URIParams:   Params{"buildpack_guid": buildpackGUID},
		Body:        body,
	})

	if err != nil {
		return nil, err
	}

	request.Header.Set("Content-Type", contentType)
	request.ContentLength = contentLength

	_, warnings, err := client.uploadBuildpackAsynchronously(request, writeErrors)
	if err != nil {
		return warnings, err
	}
	return warnings, nil

}

func (*Client) calculateBuildpackRequestSize(buildpackSize int64, bpPath string) (int64, error) {
	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)

	bpFileName := filepath.Base(bpPath)

	_, err := form.CreateFormFile("buildpack", bpFileName)
	if err != nil {
		return 0, err
	}

	err = form.Close()
	if err != nil {
		return 0, err
	}

	return int64(body.Len()) + buildpackSize, nil
}

func (*Client) createMultipartBodyAndHeaderForBuildpack(buildpack io.Reader, bpPath string) (string, io.ReadSeeker, <-chan error) {
	writerOutput, writerInput := cloudcontroller.NewPipeBomb()

	form := multipart.NewWriter(writerInput)

	writeErrors := make(chan error)

	go func() {
		defer close(writeErrors)
		defer writerInput.Close()

		bpFileName := filepath.Base(bpPath)
		writer, err := form.CreateFormFile("buildpack", bpFileName)
		if err != nil {
			writeErrors <- err
			return
		}

		_, err = io.Copy(writer, buildpack)
		if err != nil {
			writeErrors <- err
			return
		}

		err = form.Close()
		if err != nil {
			writeErrors <- err
		}
	}()

	return form.FormDataContentType(), writerOutput, writeErrors
}

func (client *Client) uploadBuildpackAsynchronously(request *cloudcontroller.Request, writeErrors <-chan error) (Buildpack, Warnings, error) {

	var buildpack Buildpack
	response := cloudcontroller.Response{
		Result: &buildpack,
	}

	httpErrors := make(chan error)

	go func() {
		defer close(httpErrors)

		err := client.connection.Make(request, &response)
		if err != nil {
			httpErrors <- err
		}
	}()

	// The following section makes the following assumptions:
	// 1) If an error occurs during file reading, an EOF is sent to the request
	// object. Thus ending the request transfer.
	// 2) If an error occurs during request transfer, an EOF is sent to the pipe.
	// Thus ending the writing routine.
	var firstError error
	var writeClosed, httpClosed bool

	for {
		select {
		case writeErr, ok := <-writeErrors:
			if !ok {
				writeClosed = true
				break // for select
			}
			if firstError == nil {
				firstError = writeErr
			}
		case httpErr, ok := <-httpErrors:
			if !ok {
				httpClosed = true
				break // for select
			}
			if firstError == nil {
				firstError = httpErr
			}
		}

		if writeClosed && httpClosed {
			break // for for
		}
	}

	return buildpack, response.Warnings, firstError
}
