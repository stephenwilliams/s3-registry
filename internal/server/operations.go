package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/stephenwilliams/s3-registry/internal/semver"
	"github.com/stephenwilliams/s3-registry/internal/store"
)

func (s *Server) registerOperations(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-tools",
		Method:      http.MethodGet,
		Path:        "/tools",
		Summary:     "List tools",
		Description: "Enumerate the tool names known to the registry.",
	}, s.listTools)

	huma.Register(api, huma.Operation{
		OperationID: "list-versions",
		Method:      http.MethodGet,
		Path:        "/tools/{tool}/versions",
		Summary:     "List versions",
		Description: "List a tool's versions, ascending by semver.",
	}, s.listVersions)

	huma.Register(api, huma.Operation{
		OperationID: "resolve-version",
		Method:      http.MethodGet,
		Path:        "/tools/{tool}/resolve",
		Summary:     "Resolve a version range",
		Description: "Resolve a semver range or 'latest' to a concrete version.",
	}, s.resolveVersion)

	huma.Register(api, huma.Operation{
		OperationID: "get-artifact",
		Method:      http.MethodGet,
		Path:        "/tools/{tool}/versions/{version}/artifact",
		Summary:     "Get a presigned artifact URL",
		Description: "Resolve the version (concrete or range/latest) and return a short-lived presigned download URL for the os-arch artifact.",
	}, s.getArtifact)
}

type listToolsOutput struct {
	Body struct {
		Tools []string `json:"tools"`
	}
}

func (s *Server) listTools(ctx context.Context, _ *struct{}) (*listToolsOutput, error) {
	tools, err := s.store.ListTools(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("failed to list tools", err)
	}
	if tools == nil {
		tools = []string{}
	}
	out := &listToolsOutput{}
	out.Body.Tools = tools
	return out, nil
}

type listVersionsInput struct {
	Tool string `path:"tool"`
}

type listVersionsOutput struct {
	Body struct {
		Versions []string `json:"versions"`
	}
}

func (s *Server) listVersions(ctx context.Context, in *listVersionsInput) (*listVersionsOutput, error) {
	idx, err := s.cache.Get(ctx, in.Tool)
	if err != nil {
		return nil, mapStoreErr(in.Tool, err)
	}
	out := &listVersionsOutput{}
	out.Body.Versions = idx.VersionStrings()
	if out.Body.Versions == nil {
		out.Body.Versions = []string{}
	}
	return out, nil
}

type resolveInput struct {
	Tool  string `path:"tool"`
	Range string `query:"range"`
}

type resolveOutput struct {
	Body struct {
		Version string `json:"version"`
	}
}

func (s *Server) resolveVersion(ctx context.Context, in *resolveInput) (*resolveOutput, error) {
	idx, err := s.cache.Get(ctx, in.Tool)
	if err != nil {
		return nil, mapStoreErr(in.Tool, err)
	}
	resolved, rerr := semver.Resolve(idx.VersionStrings(), in.Range)
	if rerr != nil {
		return nil, huma.Error422UnprocessableEntity(rerr.Error())
	}
	out := &resolveOutput{}
	out.Body.Version = resolved
	return out, nil
}

type artifactInput struct {
	Tool    string `path:"tool"`
	Version string `path:"version"`
	OS      string `query:"os"`
	Arch    string `query:"arch"`
}

type artifactOutput struct {
	Body struct {
		URL     string `json:"url"`
		SHA256  string `json:"sha256"`
		Size    int64  `json:"size"`
		Version string `json:"version"`
	}
}

func (s *Server) getArtifact(ctx context.Context, in *artifactInput) (*artifactOutput, error) {
	if in.OS == "" || in.Arch == "" {
		return nil, huma.Error422UnprocessableEntity("os and arch query parameters are required")
	}
	idx, err := s.cache.Get(ctx, in.Tool)
	if err != nil {
		return nil, mapStoreErr(in.Tool, err)
	}
	osArch := in.OS + "-" + in.Arch
	ver, art, rerr := idx.ResolveVersion(in.Version, osArch)
	if rerr != nil {
		return nil, huma.Error404NotFound(rerr.Error())
	}
	url, perr := s.store.PresignGet(ctx, art.Key, s.cfg.PresignTTL)
	if perr != nil {
		return nil, huma.Error500InternalServerError("failed to presign artifact", perr)
	}
	out := &artifactOutput{}
	out.Body.URL = url
	out.Body.SHA256 = art.SHA256
	out.Body.Size = art.Size
	out.Body.Version = ver.Version
	return out, nil
}

func mapStoreErr(tool string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return huma.Error404NotFound("unknown tool: " + tool)
	}
	return huma.Error500InternalServerError("failed to read index", err)
}
