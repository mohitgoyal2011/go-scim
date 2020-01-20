package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/elvsn/scim.go/crud"
	"github.com/elvsn/scim.go/crud/expr"
	"github.com/elvsn/scim.go/db"
	scimjson "github.com/elvsn/scim.go/json"
	"github.com/elvsn/scim.go/prop"
	"github.com/elvsn/scim.go/service/filter"
	"github.com/elvsn/scim.go/spec"
	"io"
	"io/ioutil"
)

// PatchService returns a Patch service
func PatchService(
	config *spec.ServiceProviderConfig,
	database db.DB,
	preFilters []filter.ByResource,
	postFilters []filter.ByResource,
) Patch {
	return &patchService{
		preFilters:  preFilters,
		postFilters: postFilters,
		database:    database,
		config:      config,
	}
}

type (
	Patch interface {
		Do(ctx context.Context, req *PatchRequest) (resp *PatchResponse, err error)
	}
	PatchPayload struct {
		Schemas    []string         `json:"schemas"`
		Operations []PatchOperation `json:"Operations"`
	}
	PatchOperation struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	PatchRequest struct {
		ResourceID    string
		MatchCriteria func(resource *prop.Resource) bool
		PayloadSource io.Reader
	}
	PatchResponse struct {
		Patched    bool
		Resource   *prop.Resource
		OldVersion string
	}
)

type patchService struct {
	preFilters  []filter.ByResource
	postFilters []filter.ByResource
	database    db.DB
	config      *spec.ServiceProviderConfig
}

func (s *patchService) Do(ctx context.Context, req *PatchRequest) (resp *PatchResponse, err error) {
	if err = s.checkSupport(); err != nil {
		return
	}

	patch, err := s.parseRequest(req)
	if err != nil {
		return
	}
	if err = patch.Validate(); err != nil {
		return
	}

	resource, err := s.database.Get(ctx, req.ResourceID, nil)
	if err != nil {
		return
	}

	if s.config.ETag.Supported && req.MatchCriteria != nil {
		if !req.MatchCriteria(resource) {
			err = fmt.Errorf("%w: resource does not meet pre condition", spec.ErrConflict)
			return
		}
	}

	// To save another database round trip, we use Clone to retain independent copy of the fetched resource.
	// However, because the cloned instance share subscribers, it is better to work on the original instance.
	// Hence, we assign reference to the clone, which will not be modified.
	ref := resource.Clone()

	for _, f := range s.preFilters {
		if err = f.FilterRef(ctx, resource, ref); err != nil {
			return
		}
	}

	for _, patchOp := range patch.Operations {
		switch patchOp.Op {
		case "add":
			if valueToAdd, err := patchOp.ParseValue(resource); err != nil {
				return nil, err
			} else if err := crud.Add(resource, patchOp.Path, valueToAdd); err != nil {
				return nil, err
			}
		case "replace":
			if valueToReplace, err := patchOp.ParseValue(resource); err != nil {
				return nil, err
			} else if err := crud.Replace(resource, patchOp.Path, valueToReplace); err != nil {
				return nil, err
			}
		case "remove":
			if err := crud.Delete(resource, patchOp.Path); err != nil {
				return nil, err
			}
		}
	}

	for _, f := range s.postFilters {
		if err = f.FilterRef(ctx, resource, ref); err != nil {
			return
		}
	}

	var (
		newVersion = resource.MetaVersionOrEmpty()
		oldVersion = ref.MetaVersionOrEmpty()
	)
	if newVersion == oldVersion {
		resp = &PatchResponse{
			Patched:    false,
			Resource:   ref,
			OldVersion: oldVersion,
		}
		return
	}

	if err = s.database.Replace(ctx, ref, resource); err != nil {
		return
	}

	resp = &PatchResponse{
		Patched:    true,
		Resource:   resource,
		OldVersion: oldVersion,
	}
	return
}

func (s *patchService) checkSupport() error {
	if !s.config.Patch.Supported {
		return fmt.Errorf("%w: patch operation is not supported", spec.ErrInternal)
	}
	return nil
}

func (s *patchService) parseRequest(req *PatchRequest) (*PatchPayload, error) {
	if req == nil || req.PayloadSource == nil {
		return nil, fmt.Errorf("%w: no payload for patch service", spec.ErrInternal)
	}

	raw, err := ioutil.ReadAll(req.PayloadSource)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to read request body", spec.ErrInternal)
	}

	patch := new(PatchPayload)
	if err := json.Unmarshal(raw, patch); err != nil {
		return nil, err
	}

	return patch, nil
}

func (p *PatchPayload) Validate() error {
	if len(p.Schemas) != 1 && p.Schemas[0] != "urn:ietf:params:scim:api:messages:2.0:PatchOp" {
		return fmt.Errorf("%w: invalid patch operation schema", spec.ErrInvalidSyntax)
	}

	for _, each := range p.Operations {
		switch each.Op {
		case "add":
			if len(each.Value) == 0 {
				return fmt.Errorf("%w: no value for add operation", spec.ErrInvalidSyntax)
			}
		case "replace":
			if len(each.Value) == 0 {
				return fmt.Errorf("%w: no value for replace operation", spec.ErrInvalidSyntax)
			}
		case "remove":
			if len(each.Path) == 0 {
				return fmt.Errorf("%w: no path for remove operation", spec.ErrInvalidSyntax)
			} else if len(each.Value) > 0 {
				return fmt.Errorf("%w: value is unnecessary for remove operation", spec.ErrInvalidSyntax)
			}
		default:
			return fmt.Errorf("%w: invalid patch operation", spec.ErrInvalidSyntax)
		}
	}

	return nil
}

func (o *PatchOperation) ParseValue(resource *prop.Resource) (interface{}, error) {
	var (
		head *expr.Expression
		err  error
	)
	{
		if len(o.Path) > 0 {
			head, err = expr.CompilePath(o.Path)
			if err != nil {
				return nil, err
			}
			if head.IsPath() && head.Token() == resource.ResourceType().ID() {
				head = head.Next()
			}
		}
	}

	attr := o.getTargetAttribute(resource.RootAttribute(), head)
	if attr == nil {
		return nil, fmt.Errorf("%w: path '%s' is invalid", spec.ErrInvalidPath, o.Path)
	}

	p := prop.NewProperty(attr)
	if err := scimjson.DeserializeProperty(o.Value, p, o.Op == "add"); err != nil {
		return nil, err
	}

	return p.Raw(), nil
}

func (o *PatchOperation) getTargetAttribute(parentAttr *spec.Attribute, cursor *expr.Expression) *spec.Attribute {
	if cursor == nil {
		return parentAttr
	}

	if parentAttr == nil {
		return nil
	}

	if cursor.IsRootOfFilter() {
		return o.getTargetAttribute(parentAttr, cursor.Next())
	}

	return o.getTargetAttribute(parentAttr.SubAttributeForName(cursor.Token()), cursor.Next())
}
