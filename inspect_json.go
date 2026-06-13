package main

import "sort"

type InspectJSON struct {
	SchemaVersion string             `json:"schema_version"`
	Types         []string           `json:"types"`
	SubjectTypes  []string           `json:"subject_types"`
	Relations     []InspectRelation  `json:"relations"`
	Conditions    []InspectCondition `json:"conditions,omitempty"`
}

type InspectRelation struct {
	Key        string              `json:"key"`
	Type       string              `json:"type"`
	Relation   string              `json:"relation"`
	Tags       []string            `json:"tags,omitempty"`
	DirectRefs []RelationReference `json:"direct_refs,omitempty"`
}

type InspectCondition struct {
	Name              string   `json:"name"`
	Expression        string   `json:"expression"`
	TupleSideParams   []string `json:"tuple_side_params"`
	RequestSideParams []string `json:"request_side_params"`
}

func inspectProjection(a *Analysis, cfg *Config) InspectJSON {
	contextual := contextualSet(cfg)
	out := InspectJSON{
		SchemaVersion: a.Model.SchemaVersion,
		Types:         append([]string{}, a.Types...),
		SubjectTypes:  append([]string{}, a.SubjectTypes...),
	}
	sort.Strings(out.Types)
	sort.Strings(out.SubjectTypes)
	for _, tr := range a.AllRelations {
		key := tr.Key()
		var tags []string
		if len(a.DirectRefs[tr.Type][tr.Relation]) > 0 {
			tags = append(tags, "assignable")
		}
		if a.Conditioned[key] {
			tags = append(tags, "CEL")
		}
		if contextual[key] {
			tags = append(tags, "contextual")
		}
		out.Relations = append(out.Relations, InspectRelation{
			Key:        key,
			Type:       tr.Type,
			Relation:   tr.Relation,
			Tags:       tags,
			DirectRefs: append([]RelationReference{}, a.DirectRefs[tr.Type][tr.Relation]...),
		})
	}
	names := make([]string, 0, len(a.Model.Conditions))
	for name := range a.Model.Conditions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := a.Model.Conditions[name]
		tupleSide, requestSide := a.TupleContextParams(name, cfg)
		out.Conditions = append(out.Conditions, InspectCondition{
			Name:              name,
			Expression:        c.Expression,
			TupleSideParams:   tupleSide,
			RequestSideParams: requestSide,
		})
	}
	return out
}
