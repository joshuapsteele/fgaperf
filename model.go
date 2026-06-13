package main

// model.go parses a compiled OpenFGA authorization model (.json) and derives
// the facts the generator needs: which (type, relation) pairs accept direct
// tuples, which user types (with optional usersets, wildcards, and conditions)
// each accepts, what parameters each CEL condition declares, and which
// relations can involve CEL evaluation anywhere in their resolution tree.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

type Model struct {
	SchemaVersion   string               `json:"schema_version"`
	TypeDefinitions []TypeDefinition     `json:"type_definitions"`
	Conditions      map[string]Condition `json:"conditions,omitempty"`
}

type TypeDefinition struct {
	Type      string             `json:"type"`
	Relations map[string]Userset `json:"relations,omitempty"`
	Metadata  *TypeMetadata      `json:"metadata,omitempty"`
}

type TypeMetadata struct {
	Relations map[string]RelationMetadata `json:"relations,omitempty"`
}

type RelationMetadata struct {
	DirectlyRelatedUserTypes []RelationReference `json:"directly_related_user_types,omitempty"`
}

type RelationReference struct {
	Type      string    `json:"type"`
	Relation  string    `json:"relation,omitempty"`
	Wildcard  *struct{} `json:"wildcard,omitempty"`
	Condition string    `json:"condition,omitempty"`
}

type Userset struct {
	This            *struct{}       `json:"this,omitempty"`
	ComputedUserset *ObjectRelation `json:"computedUserset,omitempty"`
	TupleToUserset  *TupleToUserset `json:"tupleToUserset,omitempty"`
	Union           *Usersets       `json:"union,omitempty"`
	Intersection    *Usersets       `json:"intersection,omitempty"`
	Difference      *Difference     `json:"difference,omitempty"`
}

type ObjectRelation struct {
	Object   string `json:"object,omitempty"`
	Relation string `json:"relation,omitempty"`
}

type TupleToUserset struct {
	Tupleset        ObjectRelation `json:"tupleset"`
	ComputedUserset ObjectRelation `json:"computedUserset"`
}

type Usersets struct {
	Child []Userset `json:"child"`
}

type Difference struct {
	Base     Userset `json:"base"`
	Subtract Userset `json:"subtract"`
}

type Condition struct {
	Name       string                  `json:"name"`
	Expression string                  `json:"expression"`
	Parameters map[string]ParamTypeRef `json:"parameters,omitempty"`
}

type ParamTypeRef struct {
	TypeName     string         `json:"type_name"`
	GenericTypes []ParamTypeRef `json:"generic_types,omitempty"`
}

// Analysis is the digested view of the model.
type Analysis struct {
	Model    *Model
	RawModel json.RawMessage
	Types    []string
	TypeDefs map[string]*TypeDefinition
	// DirectRefs[type][relation] lists assignable user types for that relation.
	DirectRefs map[string]map[string][]RelationReference
	// AllRelations lists every (type, relation) pair, assignable or computed.
	AllRelations []TypeRelation
	// Conditioned[type#relation] is true when resolving that relation can
	// require evaluating a CEL condition somewhere in its tree.
	Conditioned map[string]bool
	// SubjectTypes are terminal types (no relations of their own) that appear
	// as plain direct user types somewhere; these become check subjects.
	SubjectTypes []string
}

type TypeRelation struct {
	Type     string
	Relation string
}

func (tr TypeRelation) Key() string { return tr.Type + "#" + tr.Relation }

func hasPlainDirectRef(refs []RelationReference) bool {
	for _, ref := range refs {
		if ref.Relation == "" && ref.Wildcard == nil {
			return true
		}
	}
	return false
}

func LoadModel(path string) (*Analysis, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading model file: %w", err)
	}
	var m Model
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parsing model JSON: %w", err)
	}
	a := &Analysis{
		Model:      &m,
		RawModel:   raw,
		TypeDefs:   map[string]*TypeDefinition{},
		DirectRefs: map[string]map[string][]RelationReference{},
	}
	for i := range m.TypeDefinitions {
		td := &m.TypeDefinitions[i]
		a.Types = append(a.Types, td.Type)
		a.TypeDefs[td.Type] = td
		refs := map[string][]RelationReference{}
		for rel := range td.Relations {
			a.AllRelations = append(a.AllRelations, TypeRelation{td.Type, rel})
			if td.Metadata != nil {
				if rm, ok := td.Metadata.Relations[rel]; ok && len(rm.DirectlyRelatedUserTypes) > 0 {
					refs[rel] = rm.DirectlyRelatedUserTypes
				}
			}
		}
		a.DirectRefs[td.Type] = refs
	}
	sort.Slice(a.AllRelations, func(i, j int) bool { return a.AllRelations[i].Key() < a.AllRelations[j].Key() })
	a.computeConditioned()
	a.computeSubjectTypes()
	return a, nil
}

// computeConditioned runs a fixpoint over the relation rewrite graph: a
// relation is "conditioned" if any of its direct user types carries a
// condition, or if any relation it rewrites to is conditioned.
func (a *Analysis) computeConditioned() {
	cond := map[string]bool{}
	// Seed: direct condition references.
	for t, rels := range a.DirectRefs {
		for rel, refs := range rels {
			for _, r := range refs {
				if r.Condition != "" {
					cond[t+"#"+rel] = true
				}
			}
		}
	}
	changed := true
	for changed {
		changed = false
		for _, tr := range a.AllRelations {
			if cond[tr.Key()] {
				continue
			}
			td := a.TypeDefs[tr.Type]
			us := td.Relations[tr.Relation]
			if a.usersetConditioned(tr.Type, tr.Relation, &us, cond) {
				cond[tr.Key()] = true
				changed = true
			}
		}
	}
	a.Conditioned = cond
}

func (a *Analysis) usersetConditioned(objType, relation string, us *Userset, cond map[string]bool) bool {
	switch {
	case us.This != nil:
		// Direct userset references (e.g. group#member) pull in the
		// referenced relation's tree.
		for _, ref := range a.DirectRefs[objType][relation] {
			if ref.Relation != "" && cond[ref.Type+"#"+ref.Relation] {
				return true
			}
		}
	case us.ComputedUserset != nil:
		return cond[objType+"#"+us.ComputedUserset.Relation]
	case us.TupleToUserset != nil:
		ts := us.TupleToUserset.Tupleset.Relation
		cu := us.TupleToUserset.ComputedUserset.Relation
		// The tupleset tuples themselves may be conditioned...
		if cond[objType+"#"+ts] {
			return true
		}
		// ...and the computed relation is evaluated on whatever object types
		// the tupleset can point at.
		for _, ref := range a.DirectRefs[objType][ts] {
			if cond[ref.Type+"#"+cu] {
				return true
			}
		}
	case us.Union != nil:
		for i := range us.Union.Child {
			if a.usersetConditioned(objType, relation, &us.Union.Child[i], cond) {
				return true
			}
		}
	case us.Intersection != nil:
		for i := range us.Intersection.Child {
			if a.usersetConditioned(objType, relation, &us.Intersection.Child[i], cond) {
				return true
			}
		}
	case us.Difference != nil:
		if a.usersetConditioned(objType, relation, &us.Difference.Base, cond) {
			return true
		}
		return a.usersetConditioned(objType, relation, &us.Difference.Subtract, cond)
	}
	return false
}

func (a *Analysis) computeSubjectTypes() {
	seen := map[string]bool{}
	for _, rels := range a.DirectRefs {
		for _, refs := range rels {
			for _, r := range refs {
				if r.Relation == "" {
					if td, ok := a.TypeDefs[r.Type]; ok && len(td.Relations) == 0 {
						seen[r.Type] = true
					}
				}
			}
		}
	}
	for t := range seen {
		a.SubjectTypes = append(a.SubjectTypes, t)
	}
	sort.Strings(a.SubjectTypes)
}

// TupleContextParams decides, per condition, which declared parameters are
// bound in tuple context at write time versus supplied in request context at
// check time. Heuristic default: structured parameters (maps, lists) ride on
// the tuple, scalars come from the request. Config can override either list.
func (a *Analysis) TupleContextParams(condName string, cfg *Config) (tupleSide, requestSide []string) {
	c, ok := a.Model.Conditions[condName]
	if !ok {
		return nil, nil
	}
	override, hasOverride := cfg.Conditions[condName]
	names := make([]string, 0, len(c.Parameters))
	for p := range c.Parameters {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		if hasOverride && (override.tupleParamsSet || len(override.TupleParams) > 0) {
			if contains(override.TupleParams, p) {
				tupleSide = append(tupleSide, p)
			} else {
				requestSide = append(requestSide, p)
			}
			continue
		}
		switch c.Parameters[p].TypeName {
		case "TYPE_NAME_MAP", "TYPE_NAME_LIST":
			tupleSide = append(tupleSide, p)
		default:
			requestSide = append(requestSide, p)
		}
	}
	return tupleSide, requestSide
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
