package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
)

const (
	maxQueryBytes       = 256 << 10
	maxDashboardItems   = 200
	maxDeletionIDs      = 1000
	maxDeletionIDChars  = 512
	maxInsightsPerProj  = 2000
	maxDashboardsPerPro = 500
)

func validKind(kind string) bool {
	return kind == "trends" || kind == "funnel" || kind == "retention" || kind == "sql"
}

type insightInput struct {
	Name  string          `json:"name"`
	Kind  *string         `json:"kind"`
	Query json.RawMessage `json:"query"`
}

// check validates the input; kind falls back to current (the stored kind on
// PUT, "" on POST).
func (in *insightInput) check(current string) (db.Insight, error) {
	var out db.Insight
	var err error
	if out.Name, err = cleanText("name", in.Name, 1, 200); err != nil {
		return out, err
	}
	out.Kind = current
	if in.Kind != nil {
		out.Kind = *in.Kind
	}
	if !validKind(out.Kind) {
		return out, errBadRequest(`"kind": trends | funnel | retention | sql`)
	}
	q := bytes.TrimSpace(in.Query)
	if len(q) == 0 || q[0] != '{' {
		return out, errBadRequest(`"query" bir JSON nesnesi olmalıdır`)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, q); err != nil {
		return out, errBadRequest(`"query" bir JSON nesnesi olmalıdır`)
	}
	if compact.Len() > maxQueryBytes {
		return out, errBadRequest(`"query" en çok %d bayt olabilir`, maxQueryBytes)
	}
	out.Query = compact.Bytes()
	return out, nil
}

func (c *reqCtx) pathID() (int64, error) {
	id, ok := parseID(c.params["id"])
	if !ok {
		return 0, errNotFound()
	}
	return id, nil
}

func notFoundOr(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return errNotFound()
	}
	return err
}

// GET /api/projects/{pid}/insights
func (s *Server) listInsights(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	return db.ListInsights(c.ctx(), s.db.R(), p.ID)
}

// POST /api/projects/{pid}/insights
func (s *Server) createInsight(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleMember)
	if err != nil {
		return nil, err
	}
	var in insightInput
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	ins, err := in.check("")
	if err != nil {
		return nil, err
	}
	ins.CreatedBy = c.actor()
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		ids, err := db.InsightIDs(c.ctx(), tx, p.ID)
		if err != nil {
			return err
		}
		if len(ids) >= maxInsightsPerProj {
			return errCode(409, "limit_exceeded", "Bir projede en çok "+strconv.Itoa(maxInsightsPerProj)+" insight olabilir")
		}
		if ins, err = db.CreateInsight(c.ctx(), tx, p.ID, ins, c.nowMs()); err != nil {
			return err
		}
		return c.audit(tx, p.OrgID, "insight.create", strconv.FormatInt(ins.ID, 10), map[string]any{"project_id": p.ID, "name": ins.Name, "kind": ins.Kind})
	})
	if err != nil {
		return nil, err
	}
	return ins, nil
}

// GET /api/projects/{pid}/insights/{id}
func (s *Server) getInsight(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	id, err := c.pathID()
	if err != nil {
		return nil, err
	}
	ins, err := db.InsightByID(c.ctx(), s.db.R(), p.ID, id)
	if err != nil {
		return nil, notFoundOr(err)
	}
	return ins, nil
}

// PUT /api/projects/{pid}/insights/{id}
func (s *Server) putInsight(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleMember)
	if err != nil {
		return nil, err
	}
	id, err := c.pathID()
	if err != nil {
		return nil, err
	}
	var in insightInput
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	var out db.Insight
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		cur, err := db.InsightByID(c.ctx(), tx, p.ID, id)
		if err != nil {
			return notFoundOr(err)
		}
		ins, err := in.check(cur.Kind)
		if err != nil {
			return err
		}
		ins.ID = id
		if out, err = db.UpdateInsight(c.ctx(), tx, p.ID, ins, c.nowMs()); err != nil {
			return notFoundOr(err)
		}
		return c.audit(tx, p.OrgID, "insight.update", strconv.FormatInt(id, 10), map[string]any{"project_id": p.ID, "name": out.Name, "kind": out.Kind})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DELETE /api/projects/{pid}/insights/{id}
func (s *Server) deleteInsight(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleMember)
	if err != nil {
		return nil, err
	}
	id, err := c.pathID()
	if err != nil {
		return nil, err
	}
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		if err := db.DeleteInsight(c.ctx(), tx, p.ID, id, c.nowMs()); err != nil {
			return notFoundOr(err)
		}
		return c.audit(tx, p.OrgID, "insight.delete", strconv.FormatInt(id, 10), map[string]any{"project_id": p.ID})
	})
	return nil, err
}

// ---- dashboards ----

type dashboardInput struct {
	Name  string `json:"name"`
	Items []struct {
		InsightID *flexInt `json:"insight_id"`
		X         *int64   `json:"x"`
		Y         *int64   `json:"y"`
		W         *int64   `json:"w"`
		H         *int64   `json:"h"`
	} `json:"items"`
}

func (in *dashboardInput) check(insights map[int64]bool) (db.Dashboard, error) {
	var out db.Dashboard
	var err error
	if out.Name, err = cleanText("name", in.Name, 1, 200); err != nil {
		return out, err
	}
	if len(in.Items) > maxDashboardItems {
		return out, errBadRequest(`"items" en çok %d öğe içerebilir`, maxDashboardItems)
	}
	out.Items = make([]db.DashboardItem, 0, len(in.Items))
	for i, it := range in.Items {
		if it.InsightID == nil || !insights[int64(*it.InsightID)] {
			return out, errBadRequest("items[%d]: bilinmeyen insight", i)
		}
		item := db.DashboardItem{InsightID: int64(*it.InsightID)}
		for _, f := range []struct {
			name string
			src  *int64
			dst  *int
		}{{"x", it.X, &item.X}, {"y", it.Y, &item.Y}, {"w", it.W, &item.W}, {"h", it.H, &item.H}} {
			if f.src == nil || *f.src < 0 || *f.src > 10000 {
				return out, errBadRequest("items[%d].%s 0 ile 10000 arasında bir tam sayı olmalıdır", i, f.name)
			}
			*f.dst = int(*f.src)
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// GET /api/projects/{pid}/dashboards
func (s *Server) listDashboards(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	return db.ListDashboards(c.ctx(), s.db.R(), p.ID)
}

// POST /api/projects/{pid}/dashboards
func (s *Server) createDashboard(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleMember)
	if err != nil {
		return nil, err
	}
	var in dashboardInput
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	var out db.Dashboard
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		ids, err := db.InsightIDs(c.ctx(), tx, p.ID)
		if err != nil {
			return err
		}
		d, err := in.check(ids)
		if err != nil {
			return err
		}
		existing, err := db.ListDashboards(c.ctx(), tx, p.ID)
		if err != nil {
			return err
		}
		if len(existing) >= maxDashboardsPerPro {
			return errCode(409, "limit_exceeded", "Bir projede en çok "+strconv.Itoa(maxDashboardsPerPro)+" dashboard olabilir")
		}
		if out, err = db.CreateDashboard(c.ctx(), tx, p.ID, d, c.nowMs()); err != nil {
			return err
		}
		return c.audit(tx, p.OrgID, "dashboard.create", strconv.FormatInt(out.ID, 10), map[string]any{"project_id": p.ID, "name": out.Name})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GET /api/projects/{pid}/dashboards/{id}
func (s *Server) getDashboard(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	id, err := c.pathID()
	if err != nil {
		return nil, err
	}
	d, err := db.DashboardByID(c.ctx(), s.db.R(), p.ID, id)
	if err != nil {
		return nil, notFoundOr(err)
	}
	return d, nil
}

// PUT /api/projects/{pid}/dashboards/{id}
func (s *Server) putDashboard(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleMember)
	if err != nil {
		return nil, err
	}
	id, err := c.pathID()
	if err != nil {
		return nil, err
	}
	var in dashboardInput
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	var out db.Dashboard
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		ids, err := db.InsightIDs(c.ctx(), tx, p.ID)
		if err != nil {
			return err
		}
		d, err := in.check(ids)
		if err != nil {
			return err
		}
		d.ID = id
		if out, err = db.UpdateDashboard(c.ctx(), tx, p.ID, d, c.nowMs()); err != nil {
			return notFoundOr(err)
		}
		return c.audit(tx, p.OrgID, "dashboard.update", strconv.FormatInt(id, 10), map[string]any{"project_id": p.ID, "name": out.Name})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DELETE /api/projects/{pid}/dashboards/{id}
func (s *Server) deleteDashboard(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleMember)
	if err != nil {
		return nil, err
	}
	id, err := c.pathID()
	if err != nil {
		return nil, err
	}
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		if err := db.DeleteDashboard(c.ctx(), tx, p.ID, id); err != nil {
			return notFoundOr(err)
		}
		return c.audit(tx, p.OrgID, "dashboard.delete", strconv.FormatInt(id, 10), map[string]any{"project_id": p.ID})
	})
	return nil, err
}

// ---- deletion requests ----

// GET /api/projects/{pid}/deletions
func (s *Server) listDeletions(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	return db.ListDeletions(c.ctx(), s.db.R(), p.ID)
}

// POST /api/projects/{pid}/deletions — records the request; the publisher
// writes tombstones/p=<pid>/<ts>-<id>.json within the publish deadline.
func (s *Server) createDeletion(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	var in struct {
		UserIDs []flexString `json:"user_ids"`
		AnonIDs []flexString `json:"anon_ids"`
		Note    string       `json:"note"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	toStrings := func(in []flexString) []string {
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = string(s)
		}
		return out
	}
	d := db.Deletion{ID: "del_" + auth.NewULID(c.now), ProjectID: p.ID, CreatedBy: c.actor()}
	if d.UserIDs, err = cleanStrings("user_ids", toStrings(in.UserIDs), maxDeletionIDs, maxDeletionIDChars); err != nil {
		return nil, err
	}
	if d.AnonIDs, err = cleanStrings("anon_ids", toStrings(in.AnonIDs), maxDeletionIDs, maxDeletionIDChars); err != nil {
		return nil, err
	}
	if len(d.UserIDs) == 0 && len(d.AnonIDs) == 0 {
		return nil, errBadRequest("En az bir kullanıcı ya da anonim kimlik gerekli")
	}
	if d.Note, err = cleanText("note", in.Note, 0, 1000); err != nil {
		return nil, err
	}
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		if d, err = db.CreateDeletion(c.ctx(), tx, d, c.nowMs()); err != nil {
			return err
		}
		// Counts only: the ids are personal data and stay out of the audit log.
		return c.audit(tx, p.OrgID, "deletion.create", d.ID, map[string]any{"project_id": p.ID, "users": len(d.UserIDs), "anons": len(d.AnonIDs)})
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return d, nil
}
