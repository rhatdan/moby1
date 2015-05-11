package distribution

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/distribution/reference"
	"github.com/docker/distribution/registry/client/transport"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/image"
	"github.com/docker/docker/image/v1"
	"github.com/docker/docker/registry"
	tagpkg "github.com/docker/docker/tag"
)

type v1ManifestFetcher struct {
	endpoint registry.APIEndpoint
	config   *InspectConfig
	repoInfo *registry.RepositoryInfo
	session  *registry.Session
}

func (mf *v1ManifestFetcher) Fetch(ref reference.Named) (imgInspect *types.RemoteImageInspect, fallback bool, err error) {
	if _, isDigested := ref.(reference.Digested); isDigested {
		// Allowing fallback, because HTTPS v1 is before HTTP v2
		return nil, true, registry.ErrNoSupport{errors.New("Cannot pull by digest with v1 registry")}
	}
	tag := ""
	if tagged, isTagged := ref.(reference.Tagged); isTagged {
		tag = tagged.Tag()
	}
	tlsConfig, err := mf.config.RegistryService.TLSConfig(mf.repoInfo.Index.Name)
	if err != nil {
		return nil, false, err
	}
	// Adds Docker-specific headers as well as user-specified headers (metaHeaders)
	tr := transport.NewTransport(
		// TODO(tiborvass): was ReceiveTimeout
		registry.NewTransport(tlsConfig),
		registry.DockerHeaders(mf.config.MetaHeaders)...,
	)
	client := registry.HTTPClient(tr)
	v1Endpoint, err := mf.endpoint.ToV1Endpoint(mf.config.MetaHeaders)
	if err != nil {
		logrus.Debugf("Could not get v1 endpoint: %v", err)
		return nil, true, err
	}
	mf.session, err = registry.NewSession(client, mf.config.AuthConfig, v1Endpoint)
	if err != nil {
		// TODO(dmcgowan): Check if should fallback
		logrus.Debugf("Fallback from error: %s", err)
		return nil, true, err
	}
	imgInspect, err = mf.fetchWithSession(tag)
	return
}

func (mf *v1ManifestFetcher) fetchWithSession(askedTag string) (*types.RemoteImageInspect, error) {
	repoData, err := mf.session.GetRepositoryData(mf.repoInfo.RemoteName)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP code: 404") {
			return nil, fmt.Errorf("Error: image %s not found", mf.repoInfo.RemoteName)
		}
		// Unexpected HTTP error
		return nil, err
	}

	logrus.Debugf("Retrieving the tag list from V1 endpoints")
	tagsList, err := mf.session.GetRemoteTags(repoData.Endpoints, mf.repoInfo.RemoteName)
	if err != nil {
		logrus.Errorf("Unable to get remote tags: %s", err)
		return nil, err
	}
	if len(tagsList) < 1 {
		return nil, fmt.Errorf("No tags available for remote repository %s", mf.repoInfo.CanonicalName)
	}

	for tag, id := range tagsList {
		repoData.ImgList[id] = &registry.ImgData{
			ID:       id,
			Tag:      tag,
			Checksum: "",
		}
	}

	// If no tag has been specified, choose `latest` if it exists
	if askedTag == "" {
		if _, exists := tagsList[tagpkg.DefaultTag]; exists {
			askedTag = tagpkg.DefaultTag
		}
	}
	if askedTag == "" {
		// fallback to any tag in the repository
		for tag := range tagsList {
			askedTag = tag
			break
		}
	}

	id, exists := tagsList[askedTag]
	if !exists {
		return nil, fmt.Errorf("Tag %s not found in repository %s", askedTag, mf.repoInfo.CanonicalName)
	}
	img := repoData.ImgList[id]

	var pulledImg *image.Image
	for _, ep := range mf.repoInfo.Index.Mirrors {
		if pulledImg, err = mf.pullImageJSON(img.ID, ep, repoData.Tokens); err != nil {
			// Don't report errors when pulling from mirrors.
			logrus.Debugf("Error pulling image json of %s:%s, mirror: %s, %s", mf.repoInfo.CanonicalName, img.Tag, ep, err)
			continue
		}
		break
	}
	if pulledImg == nil {
		for _, ep := range repoData.Endpoints {
			if pulledImg, err = mf.pullImageJSON(img.ID, ep, repoData.Tokens); err != nil {
				// It's not ideal that only the last error is returned, it would be better to concatenate the errors.
				logrus.Infof("Error pulling image json of %s:%s, endpoint: %s, %v", mf.repoInfo.CanonicalName, img.Tag, ep, err)
				continue
			}
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("Error pulling image (%s) from %s, %v", img.Tag, mf.repoInfo.CanonicalName, err)
	}
	if pulledImg == nil {
		return nil, fmt.Errorf("No such image %s:%s", mf.repoInfo.CanonicalName, askedTag)
	}

	return makeRemoteImageInspect(mf.repoInfo, pulledImg, askedTag, ""), nil
}

func (mf *v1ManifestFetcher) pullImageJSON(imgID, endpoint string, token []string) (*image.Image, error) {
	imgJSON, _, err := mf.session.GetRemoteImageJSON(imgID, endpoint)
	if err != nil {
		return nil, err
	}
	h, err := v1.HistoryFromConfig(imgJSON, false)
	if err != nil {
		return nil, err
	}
	configRaw, err := v1.MakeRawConfigFromV1Config(imgJSON, image.NewRootFS(), []image.History{h})
	if err != nil {
		return nil, err
	}
	config, err := json.Marshal(configRaw)
	if err != nil {
		return nil, err
	}
	img, err := image.NewFromJSON(config)
	if err != nil {
		return nil, err
	}
	return img, nil
}
