package generate

import (
	"github.com/vektah/gqlparser/v2/ast"
)

// pruneSchema extracts a minimal schema containing only the types and fields
// reachable from the given operations document.  This reduces generated code
// size when working with large schemas where only a small subset of types is
// actually queried.
func pruneSchema(schema *ast.Schema, doc *ast.QueryDocument) *ast.Schema {
	e := newExtractor(schema)
	e.walkDocument(doc)
	e.resolve()
	return e.build()
}

// extractor walks GraphQL operations to determine the minimal set of schema
// types and fields needed to serve them.
type extractor struct {
	schema *ast.Schema
	// neededFields tracks which fields of each type are required.
	// A type present as a key (even with an empty map) is considered needed.
	neededFields map[string]map[string]bool
	// fullTypes are types whose complete field set must be preserved
	// (e.g. input objects).
	fullTypes map[string]bool
}

func newExtractor(schema *ast.Schema) *extractor {
	return &extractor{
		schema:       schema,
		neededFields: make(map[string]map[string]bool),
		fullTypes:    make(map[string]bool),
	}
}

func (e *extractor) markNeeded(typeName string) {
	if e.neededFields[typeName] == nil {
		e.neededFields[typeName] = make(map[string]bool)
	}
}

func (e *extractor) addField(typeName, fieldName string) {
	e.markNeeded(typeName)
	e.neededFields[typeName][fieldName] = true
}

func (e *extractor) isNeeded(typeName string) bool {
	return e.neededFields[typeName] != nil || e.fullTypes[typeName]
}

func (e *extractor) addFullType(typeName string) {
	e.fullTypes[typeName] = true
}

// namedType resolves wrapped types (lists, non-null) to the underlying named
// type.
func namedType(t *ast.Type) string {
	for t.Elem != nil {
		t = t.Elem
	}
	return t.NamedType
}

// walkDocument seeds the needed types/fields from all operations and
// fragments.
func (e *extractor) walkDocument(doc *ast.QueryDocument) {
	for _, op := range doc.Operations {
		for _, v := range op.VariableDefinitions {
			tn := namedType(v.Type)
			if def := e.schema.Types[tn]; def != nil && def.Kind == ast.InputObject {
				e.addFullType(tn)
			}
		}
		rootType := "Query"
		switch op.Operation {
		case ast.Mutation:
			rootType = "Mutation"
		case ast.Subscription:
			rootType = "Subscription"
		}
		e.markNeeded(rootType)
		e.walkSelections(op.SelectionSet, rootType)
	}
	for _, frag := range doc.Fragments {
		e.markNeeded(frag.TypeCondition)
		e.walkSelections(frag.SelectionSet, frag.TypeCondition)
	}
}

// walkSelections recursively collects field references from a selection set.
func (e *extractor) walkSelections(sels ast.SelectionSet, parentType string) {
	parentDef := e.schema.Types[parentType]
	if parentDef == nil {
		return
	}
	for _, sel := range sels {
		switch sel := sel.(type) {
		case *ast.Field:
			e.addField(parentType, sel.Name)
			fd := parentDef.Fields.ForName(sel.Name)
			if fd == nil {
				continue
			}
			retType := namedType(fd.Type)
			e.markNeeded(retType)
			if len(sel.SelectionSet) > 0 {
				e.walkSelections(sel.SelectionSet, retType)
			}
		case *ast.FragmentSpread:
			if sel.Definition != nil {
				tc := sel.Definition.TypeCondition
				e.markNeeded(tc)
				e.walkSelections(sel.Definition.SelectionSet, tc)
			}
		case *ast.InlineFragment:
			tc := sel.TypeCondition
			if tc == "" {
				tc = parentType
			}
			e.markNeeded(tc)
			e.walkSelections(sel.SelectionSet, tc)
		}
	}
}

// resolve transitively closes over the needed types by following field return
// types, argument types, interface implementations, and full-type expansions.
func (e *extractor) resolve() {
	for changed := true; changed; {
		changed = e.resolveFullTypes() ||
			e.resolveAbstractTypes() ||
			e.resolveNeededFields()
	}
}

// resolveFullTypes expands full types (input objects) so that all their
// fields' types are marked as needed.  Returns true if any new types were
// discovered.
func (e *extractor) resolveFullTypes() bool {
	changed := false
	for tn := range e.fullTypes {
		def := e.schema.Types[tn]
		if def == nil {
			continue
		}
		for _, fd := range def.Fields {
			ft := namedType(fd.Type)
			changed = e.ensureNeeded(ft) || changed
			changed = e.ensureFullInputType(ft) || changed
		}
	}
	return changed
}

// resolveAbstractTypes ensures that all implementations of needed interfaces
// and all members of needed unions are themselves marked as needed.  The code
// generator calls schema.GetPossibleTypes to enumerate concrete types for
// unmarshaling, so every implementation must be present in the pruned schema.
func (e *extractor) resolveAbstractTypes() bool {
	changed := false
	for tn := range e.neededFields {
		def := e.schema.Types[tn]
		if def == nil {
			continue
		}
		switch def.Kind {
		case ast.Interface, ast.Union:
			for _, impl := range e.schema.PossibleTypes[tn] {
				changed = e.ensureNeeded(impl.Name) || changed
			}
		}
	}
	return changed
}

// resolveNeededFields ensures field return types and argument types are
// present, and propagates fields between interfaces and their
// implementations.  Returns true if any new types or fields were discovered.
func (e *extractor) resolveNeededFields() bool {
	changed := false
	for tn, fields := range e.neededFields {
		def := e.schema.Types[tn]
		if def == nil {
			continue
		}
		changed = e.resolveFieldDeps(def, fields) || changed
		changed = e.propagateFields(tn, def, fields) || changed
	}
	return changed
}

// resolveFieldDeps ensures the return type and argument types of each needed
// field are themselves marked as needed.
func (e *extractor) resolveFieldDeps(def *ast.Definition, fields map[string]bool) bool {
	changed := false
	for fn := range fields {
		fd := def.Fields.ForName(fn)
		if fd == nil {
			continue
		}
		changed = e.ensureNeeded(namedType(fd.Type)) || changed
		for _, arg := range fd.Arguments {
			changed = e.ensureArgType(namedType(arg.Type)) || changed
		}
	}
	return changed
}

// propagateFields copies needed field entries between an object and the
// interfaces it implements (in both directions).
func (e *extractor) propagateFields(typeName string, def *ast.Definition, fields map[string]bool) bool {
	switch def.Kind {
	case ast.Object:
		return e.propagateFieldsToInterfaces(def, fields)
	case ast.Interface:
		return e.propagateFieldsToImplementors(typeName, fields)
	default:
		return false
	}
}

// propagateFieldsToInterfaces copies needed fields from an object type to
// any of its interfaces that are also needed.
func (e *extractor) propagateFieldsToInterfaces(def *ast.Definition, fields map[string]bool) bool {
	changed := false
	for _, iface := range def.Interfaces {
		if !e.isNeeded(iface) {
			continue
		}
		ifaceDef := e.schema.Types[iface]
		if ifaceDef == nil {
			continue
		}
		for fn := range fields {
			if ifaceDef.Fields.ForName(fn) == nil || e.neededFields[iface][fn] {
				continue
			}
			e.addField(iface, fn)
			changed = true
		}
	}
	return changed
}

// propagateFieldsToImplementors copies needed fields from an interface to
// its implementing types that are also needed.
func (e *extractor) propagateFieldsToImplementors(typeName string, fields map[string]bool) bool {
	changed := false
	for _, impl := range e.schema.PossibleTypes[typeName] {
		if !e.isNeeded(impl.Name) {
			continue
		}
		for fn := range fields {
			if impl.Fields.ForName(fn) == nil || e.neededFields[impl.Name][fn] {
				continue
			}
			e.addField(impl.Name, fn)
			changed = true
		}
	}
	return changed
}

// ensureNeeded marks a type as needed if it isn't already. Returns true if
// the type was newly marked.
func (e *extractor) ensureNeeded(typeName string) bool {
	if e.isNeeded(typeName) {
		return false
	}
	e.markNeeded(typeName)
	return true
}

// ensureFullInputType marks a type as a full type if it is an input object
// that hasn't been marked yet.  Returns true if newly marked.
func (e *extractor) ensureFullInputType(typeName string) bool {
	if e.fullTypes[typeName] {
		return false
	}
	def := e.schema.Types[typeName]
	if def == nil || def.Kind != ast.InputObject {
		return false
	}
	e.addFullType(typeName)
	return true
}

// ensureArgType marks an argument type as needed: input objects are added as
// full types, other types are simply marked.  Returns true if any change was
// made.
func (e *extractor) ensureArgType(typeName string) bool {
	if e.isNeeded(typeName) {
		return false
	}
	def := e.schema.Types[typeName]
	if def != nil && def.Kind == ast.InputObject {
		e.addFullType(typeName)
		return true
	}
	e.markNeeded(typeName)
	return true
}

// build constructs a minimal ast.Schema containing only needed types and
// fields.
func (e *extractor) build() *ast.Schema {
	out := &ast.Schema{
		Types:         make(map[string]*ast.Definition),
		Directives:    make(map[string]*ast.DirectiveDefinition),
		PossibleTypes: make(map[string][]*ast.Definition),
		Implements:    make(map[string][]*ast.Definition),
	}

	for tn := range e.allNeededTypes() {
		orig := e.schema.Types[tn]
		if orig == nil || orig.BuiltIn {
			continue
		}
		out.Types[tn] = e.buildDefinition(orig)
	}

	out.Query = out.Types["Query"]
	out.Mutation = out.Types["Mutation"]
	out.Subscription = out.Types["Subscription"]

	e.populateRelationships(out)

	// Copy builtins so the generator can still resolve primitive types.
	for tn, def := range e.schema.Types {
		if def.BuiltIn {
			out.Types[tn] = def
		}
	}

	return out
}

func (e *extractor) allNeededTypes() map[string]bool {
	all := make(map[string]bool, len(e.neededFields)+len(e.fullTypes))
	for tn := range e.neededFields {
		all[tn] = true
	}
	for tn := range e.fullTypes {
		all[tn] = true
	}
	return all
}

func (e *extractor) buildDefinition(orig *ast.Definition) *ast.Definition {
	d := &ast.Definition{
		Kind:       orig.Kind,
		Name:       orig.Name,
		Position:   orig.Position,
		EnumValues: orig.EnumValues,
	}

	for _, iface := range orig.Interfaces {
		if e.isNeeded(iface) {
			d.Interfaces = append(d.Interfaces, iface)
		}
	}

	d.Fields = e.selectFields(orig)
	d.Types = e.selectUnionMembers(orig)

	return d
}

func (e *extractor) selectFields(orig *ast.Definition) ast.FieldList {
	switch {
	case orig.Kind == ast.Scalar || orig.Kind == ast.Enum || orig.Kind == ast.Union:
		return nil
	case e.fullTypes[orig.Name]:
		return orig.Fields
	default:
		needed := e.neededFields[orig.Name]
		var fields ast.FieldList
		for _, fd := range orig.Fields {
			if needed[fd.Name] {
				fields = append(fields, fd)
			}
		}
		return fields
	}
}

func (e *extractor) selectUnionMembers(orig *ast.Definition) []string {
	if orig.Kind != ast.Union {
		return nil
	}
	var members []string
	for _, member := range orig.Types {
		if e.isNeeded(member) {
			members = append(members, member)
		}
	}
	return members
}

// populateRelationships fills PossibleTypes and Implements on the output
// schema so that schema.GetPossibleTypes works correctly.
func (e *extractor) populateRelationships(out *ast.Schema) {
	for _, def := range out.Types {
		switch def.Kind {
		case ast.Union:
			for _, memberName := range def.Types {
				memberDef := out.Types[memberName]
				if memberDef == nil {
					continue
				}
				out.PossibleTypes[def.Name] = append(out.PossibleTypes[def.Name], memberDef)
				out.Implements[memberName] = append(out.Implements[memberName], def)
			}
		case ast.Object:
			for _, ifaceName := range def.Interfaces {
				ifaceDef := out.Types[ifaceName]
				if ifaceDef == nil {
					continue
				}
				out.PossibleTypes[ifaceName] = append(out.PossibleTypes[ifaceName], def)
				out.Implements[def.Name] = append(out.Implements[def.Name], ifaceDef)
			}
		}
	}
}
