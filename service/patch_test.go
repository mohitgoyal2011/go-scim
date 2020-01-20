package service

import (
	"context"
	"encoding/json"
	"github.com/elvsn/scim.go/db"
	"github.com/elvsn/scim.go/prop"
	"github.com/elvsn/scim.go/service/filter"
	"github.com/elvsn/scim.go/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"io/ioutil"
	"os"
	"strings"
	"testing"
)

func TestPatchService(t *testing.T) {
	s := new(PatchServiceTestSuite)
	suite.Run(t, s)
}

type PatchServiceTestSuite struct {
	suite.Suite
	resourceType *spec.ResourceType
	config       *spec.ServiceProviderConfig
}

func (s *PatchServiceTestSuite) TestDo() {
	tests := []struct {
		name       string
		setup      func(t *testing.T) Patch
		getRequest func() *PatchRequest
		expect     func(t *testing.T, resp *PatchResponse, err error)
	}{
		{
			name: "patch to make a difference",
			setup: func(t *testing.T) Patch {
				database := db.Memory()
				err := database.Insert(context.TODO(), s.resourceOf(t, map[string]interface{}{
					"schemas":  []interface{}{"urn:ietf:params:scim:schemas:core:2.0:User"},
					"id":       "foo",
					"userName": "foo",
					"timezone": "Asia/Shanghai",
					"emails": []interface{}{
						map[string]interface{}{
							"value": "foo@bar.com",
							"type":  "home",
						},
					},
				}))
				require.Nil(t, err)
				return PatchService(s.config, database, nil, []filter.ByResource{
					filter.ByPropertyToByResource(
						filter.ReadOnlyFilter(),
						filter.BCryptFilter(),
					),
					filter.ByPropertyToByResource(filter.ValidationFilter(database)),
					filter.MetaFilter(),
				})
			},
			getRequest: func() *PatchRequest {
				return &PatchRequest{
					ResourceID: "foo",
					PayloadSource: strings.NewReader(`
{
	"schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	"Operations": [
		{
			"op": "add",
			"path": "userName",
			"value": "foobar"
		},
		{
			"op": "replace",
			"path": "emails[value eq \"foo@bar.com\"].type",
			"value": "work"
		},
		{
			"op": "remove",
			"path": "timezone"
		}
	]
}
`),
				}
			},
			expect: func(t *testing.T, resp *PatchResponse, err error) {
				assert.Nil(t, err)
				assert.True(t, resp.Patched)
				assert.NotEmpty(t, resp.Resource.MetaVersionOrEmpty())
				assert.NotEqual(t, resp.OldVersion, resp.Resource.MetaVersionOrEmpty())
				assert.Equal(t, "foobar", resp.Resource.Navigator().Dot("userName").Current().Raw())
				assert.True(t, resp.Resource.Navigator().Dot("timezone").Current().IsUnassigned())
				assert.Equal(t, "work", resp.Resource.Navigator().Dot("emails").At(0).Dot("type").Current().Raw())
			},
		},
		{
			name: "patch to not make a difference",
			setup: func(t *testing.T) Patch {
				database := db.Memory()
				err := database.Insert(context.TODO(), s.resourceOf(t, map[string]interface{}{
					"schemas":  []interface{}{"urn:ietf:params:scim:schemas:core:2.0:User"},
					"id":       "foo",
					"userName": "foo",
					"timezone": "Asia/Shanghai",
					"emails": []interface{}{
						map[string]interface{}{
							"value": "foo@bar.com",
							"type":  "home",
						},
					},
				}))
				require.Nil(t, err)
				return PatchService(s.config, database, nil, []filter.ByResource{
					filter.ByPropertyToByResource(
						filter.ReadOnlyFilter(),
						filter.BCryptFilter(),
					),
					filter.ByPropertyToByResource(filter.ValidationFilter(database)),
					filter.MetaFilter(),
				})
			},
			getRequest: func() *PatchRequest {
				return &PatchRequest{
					ResourceID: "foo",
					PayloadSource: strings.NewReader(`
{
	"schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
	"Operations": [
		{
			"op": "add",
			"path": "userName",
			"value": "foo"
		}
	]
}
`),
				}
			},
			expect: func(t *testing.T, resp *PatchResponse, err error) {
				assert.Nil(t, err)
				assert.False(t, resp.Patched)
			},
		},
	}

	for _, test := range tests {
		s.T().Run(test.name, func(t *testing.T) {
			service := test.setup(t)
			resp, err := service.Do(context.TODO(), test.getRequest())
			test.expect(t, resp, err)
		})
	}
}

func (s *PatchServiceTestSuite) resourceOf(t *testing.T, data interface{}) *prop.Resource {
	r := prop.NewResource(s.resourceType)
	require.Nil(t, r.Navigator().Replace(data).Error())
	return r
}

func (s *PatchServiceTestSuite) SetupSuite() {
	for _, each := range []struct {
		filepath  string
		structure interface{}
		post      func(parsed interface{})
	}{
		{
			filepath:  "../stock/core_schema.json",
			structure: new(spec.Schema),
			post: func(parsed interface{}) {
				spec.Schemas().Register(parsed.(*spec.Schema))
			},
		},
		{
			filepath:  "../stock/user_schema.json",
			structure: new(spec.Schema),
			post: func(parsed interface{}) {
				spec.Schemas().Register(parsed.(*spec.Schema))
			},
		},
		{
			filepath:  "../stock/user_resource_type.json",
			structure: new(spec.ResourceType),
			post: func(parsed interface{}) {
				s.resourceType = parsed.(*spec.ResourceType)
			},
		},
	} {
		f, err := os.Open(each.filepath)
		require.Nil(s.T(), err)

		raw, err := ioutil.ReadAll(f)
		require.Nil(s.T(), err)

		err = json.Unmarshal(raw, each.structure)
		require.Nil(s.T(), err)

		if each.post != nil {
			each.post(each.structure)
		}
	}

	s.config = new(spec.ServiceProviderConfig)
	require.Nil(s.T(), json.Unmarshal([]byte(`
{
  "patch": {
    "supported": true
  }
}
`), s.config))
}
