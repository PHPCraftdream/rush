package sessguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/PHPCraftdream/rush/internal/session/batch/finterm"
)

const FinalizationAncestorLimit = 32

type finalizationAncestorReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type finalizationManagedEdge struct {
	Node, Owner, Child string
	Token              int64
}

type finalizationAncestry struct {
	Owners []string
	Edges  []finalizationManagedEdge
}

// FinalizationParentEdgesQuery runs once per ancestor on the writer connection,
// also inside the commit transaction; idx_batch_nodes_child_session serves it.
const FinalizationParentEdgesQuery = `SELECT node_id, owner_session_id, token FROM batch_nodes
		WHERE child_session_id=? ORDER BY node_id`

// Rows close before the caller can recurse on the single writer connection.
func readFinalizationParentEdges(ctx context.Context, reader finalizationAncestorReader, owner string) (edges []finalizationManagedEdge, retErr error) {
	rows, err := reader.QueryContext(ctx, FinalizationParentEdgesQuery, owner)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		edge := finalizationManagedEdge{Child: owner}
		if err := rows.Scan(&edge.Node, &edge.Owner, &edge.Token); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	return edges, rows.Err()
}

// ReadFinalizationAncestry follows durable delegation edges, not session forks.
// Exact edges are revalidated inside the writer, including delegation tokens.
func ReadFinalizationAncestry(ctx context.Context, reader finalizationAncestorReader, owners []string) (finalizationAncestry, error) {
	var snapshot finalizationAncestry
	parents := make(map[string][]string)
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	var visit func(string, int) error
	visit = func(owner string, depth int) error {
		if owner == "" || visiting[owner] || depth > FinalizationAncestorLimit {
			return finterm.ErrAgentFinalizationChanged
		}
		if visited[owner] {
			return nil
		}
		visiting[owner] = true
		edges, err := readFinalizationParentEdges(ctx, reader, owner)
		if err != nil {
			return fmt.Errorf("read managed parents: %w", err)
		}
		for _, edge := range edges {
			snapshot.Edges = append(snapshot.Edges, edge)
			parents[owner] = append(parents[owner], edge.Owner)
		}
		for _, parent := range parents[owner] {
			if err := visit(parent, depth+1); err != nil {
				return err
			}
		}
		visiting[owner] = false
		visited[owner] = true
		return nil
	}
	for _, owner := range owners {
		if err := visit(owner, 0); err != nil {
			return finalizationAncestry{}, err
		}
	}
	for owner := range visited {
		snapshot.Owners = append(snapshot.Owners, owner)
	}
	slices.Sort(snapshot.Owners)
	slices.SortFunc(snapshot.Edges, func(a, b finalizationManagedEdge) int {
		return strings.Compare(a.Node, b.Node)
	})
	return snapshot, nil
}
