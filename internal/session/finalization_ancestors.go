package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const finalizationAncestorLimit = 32

type finalizationAncestorReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type finalizationManagedEdge struct {
	node, owner, child string
	token              int64
}

type finalizationAncestry struct {
	owners []string
	edges  []finalizationManagedEdge
}

// readFinalizationAncestry follows durable delegation edges, not session forks.
// Exact edges are revalidated inside the writer, including delegation tokens.
func readFinalizationAncestry(ctx context.Context, reader finalizationAncestorReader, owners []string) (finalizationAncestry, error) {
	var snapshot finalizationAncestry
	parents := make(map[string][]string)
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	var visit func(string, int) error
	visit = func(owner string, depth int) error {
		if owner == "" || visiting[owner] || depth > finalizationAncestorLimit {
			return ErrAgentFinalizationChanged
		}
		if visited[owner] {
			return nil
		}
		visiting[owner] = true
		// Terminal edges still identify managed ancestry despite physical holds.
		rows, err := reader.QueryContext(ctx, `SELECT node_id, owner_session_id, token FROM batch_nodes
			WHERE child_session_id=? ORDER BY node_id`, owner)
		if err != nil {
			return fmt.Errorf("read managed parent: %w", err)
		}
		for rows.Next() {
			edge := finalizationManagedEdge{child: owner}
			if err := rows.Scan(&edge.node, &edge.owner, &edge.token); err != nil {
				_ = rows.Close()
				return err
			}
			snapshot.edges = append(snapshot.edges, edge)
			parents[owner] = append(parents[owner], edge.owner)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil || closeErr != nil {
			return fmt.Errorf("read managed parents: %w", errors.Join(err, closeErr))
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
		snapshot.owners = append(snapshot.owners, owner)
	}
	slices.Sort(snapshot.owners)
	slices.SortFunc(snapshot.edges, func(a, b finalizationManagedEdge) int {
		return strings.Compare(a.node, b.node)
	})
	return snapshot, nil
}
