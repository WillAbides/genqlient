package generate

import (
	"sort"
	"testing"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

func TestPruneSchema(t *testing.T) {
	schemaStr := `
		scalar DateTime

		enum Role { STUDENT TEACHER }

		input UserInput {
			name: String!
			role: Role
		}

		type User {
			id: ID!
			name: String!
			email: String
			role: Role
		}

		type Post {
			id: ID!
			title: String!
			body: String!
		}

		type Query {
			user(input: UserInput!): User
			posts: [Post!]!
		}
	`

	schema := mustLoadSchema(t, schemaStr)

	tests := []struct {
		name          string
		query         string
		wantTypes     []string
		dontWantTypes []string
		wantFields    map[string][]string
	}{
		{
			name:          "simple query prunes unused types",
			query:         `query GetUser { user(input: {name: "x"}) { id name } }`,
			wantTypes:     []string{"Query", "User", "UserInput", "Role"},
			dontWantTypes: []string{"Post", "DateTime"},
			wantFields: map[string][]string{
				"Query": {"user"},
				"User":  {"id", "name"},
			},
		},
		{
			name:          "different query keeps different types",
			query:         `query GetPosts { posts { id title } }`,
			wantTypes:     []string{"Query", "Post"},
			dontWantTypes: []string{"User", "UserInput", "Role", "DateTime"},
			wantFields: map[string][]string{
				"Query": {"posts"},
				"Post":  {"id", "title"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, errs := gqlparser.LoadQuery(schema, tt.query)
			if errs != nil {
				t.Fatalf("parsing query: %v", errs)
			}

			pruned := pruneSchema(schema, doc)

			for _, tn := range tt.wantTypes {
				if pruned.Types[tn] == nil {
					t.Errorf("expected type %q to be present", tn)
				}
			}
			for _, tn := range tt.dontWantTypes {
				if def := pruned.Types[tn]; def != nil && !def.BuiltIn {
					t.Errorf("expected type %q to NOT be present", tn)
				}
			}
			for tn, wantFields := range tt.wantFields {
				def := pruned.Types[tn]
				if def == nil {
					t.Errorf("expected type %q to exist for field check", tn)
					continue
				}
				gotFields := make([]string, len(def.Fields))
				for i, f := range def.Fields {
					gotFields[i] = f.Name
				}
				sort.Strings(gotFields)
				sort.Strings(wantFields)
				if len(gotFields) != len(wantFields) {
					t.Errorf("type %q: got fields %v, want %v", tn, gotFields, wantFields)
					continue
				}
				for i := range gotFields {
					if gotFields[i] != wantFields[i] {
						t.Errorf("type %q: got fields %v, want %v", tn, gotFields, wantFields)
						break
					}
				}
			}
		})
	}
}

func TestPruneSchemaWithInterfacesAndUnions(t *testing.T) {
	schemaStr := `
		interface Node {
			id: ID!
		}

		interface Content {
			id: ID!
			title: String!
		}

		type Article implements Content & Node {
			id: ID!
			title: String!
			body: String!
		}

		type Video implements Content & Node {
			id: ID!
			title: String!
			duration: Int!
		}

		type Image implements Node {
			id: ID!
			url: String!
		}

		union SearchResult = Article | Video | Image

		type Query {
			content: Content
			search: SearchResult
			node(id: ID!): Node
		}
	`

	schema := mustLoadSchema(t, schemaStr)

	t.Run("interface preserves all implementations", func(t *testing.T) {
		doc, errs := gqlparser.LoadQuery(schema, `
			query GetContent {
				content {
					id
					title
					... on Article { body }
				}
			}
		`)
		if errs != nil {
			t.Fatalf("parsing query: %v", errs)
		}

		pruned := pruneSchema(schema, doc)

		// All implementations of Content must be present (Article AND
		// Video) even though only Article has an inline fragment,
		// because the generator needs GetPossibleTypes for unmarshaling.
		for _, tn := range []string{"Content", "Article", "Video"} {
			if pruned.Types[tn] == nil {
				t.Errorf("expected type %q to be present", tn)
			}
		}
		// Image only implements Node (not Content) and Node is not
		// queried, so Image should be absent.
		if def := pruned.Types["Image"]; def != nil && !def.BuiltIn {
			t.Errorf("expected type Image to NOT be present")
		}

		// GetPossibleTypes should return both implementations.
		contentDef := pruned.Types["Content"]
		possibleTypes := pruned.GetPossibleTypes(contentDef)
		gotNames := make([]string, len(possibleTypes))
		for i, pt := range possibleTypes {
			gotNames[i] = pt.Name
		}
		sort.Strings(gotNames)
		wantNames := []string{"Article", "Video"}
		if len(gotNames) != len(wantNames) {
			t.Fatalf("Content possible types: got %v, want %v", gotNames, wantNames)
		}
		for i := range gotNames {
			if gotNames[i] != wantNames[i] {
				t.Fatalf("Content possible types: got %v, want %v", gotNames, wantNames)
			}
		}
	})

	t.Run("interface without fragments preserves all implementations", func(t *testing.T) {
		doc, errs := gqlparser.LoadQuery(schema, `
			query GetContent {
				content { id title }
			}
		`)
		if errs != nil {
			t.Fatalf("parsing query: %v", errs)
		}

		pruned := pruneSchema(schema, doc)

		// Even without any inline fragments, all implementations must
		// be present for GetPossibleTypes.
		for _, tn := range []string{"Content", "Article", "Video"} {
			if pruned.Types[tn] == nil {
				t.Errorf("expected type %q to be present", tn)
			}
		}
	})

	t.Run("union preserves all members", func(t *testing.T) {
		doc, errs := gqlparser.LoadQuery(schema, `
			query Search {
				search {
					... on Article { id title body }
				}
			}
		`)
		if errs != nil {
			t.Fatalf("parsing query: %v", errs)
		}

		pruned := pruneSchema(schema, doc)

		// All union members must be present even if only Article is
		// explicitly referenced.
		for _, tn := range []string{"SearchResult", "Article", "Video", "Image"} {
			if pruned.Types[tn] == nil {
				t.Errorf("expected type %q to be present", tn)
			}
		}
	})
}

func TestPruneSchemaPreservesBuiltins(t *testing.T) {
	schemaStr := `
		type User { id: ID!, name: String! }
		type Query { user: User }
	`
	schema := mustLoadSchema(t, schemaStr)
	doc, errs := gqlparser.LoadQuery(schema, `query { user { id name } }`)
	if errs != nil {
		t.Fatalf("parsing query: %v", errs)
	}

	pruned := pruneSchema(schema, doc)

	// Builtins like String, ID, Boolean, Int, etc. must still be present.
	for _, builtin := range []string{"String", "ID", "Boolean", "Int", "Float"} {
		if pruned.Types[builtin] == nil {
			t.Errorf("expected builtin type %q to be present", builtin)
		}
	}
}

func mustLoadSchema(t *testing.T, sdl string) *ast.Schema {
	t.Helper()
	schema, err := gqlparser.LoadSchema(&ast.Source{Name: "test.graphql", Input: sdl})
	if err != nil {
		t.Fatalf("loading schema: %v", err)
	}
	return schema
}
