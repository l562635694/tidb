// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package handle

import (
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/sqlexec"
	"github.com/pingcap/tipb/go-tipb"
)

// JSONTable is used for dumping statistics.
type JSONTable struct {
	DatabaseName string                 `json:"database_name"`
	TableName    string                 `json:"table_name"`
	Columns      map[string]*jsonColumn `json:"columns"`
	Indices      map[string]*jsonColumn `json:"indices"`
	ExtStats     []*jsonExtendedStats   `json:"ext_stats"`
	Count        int64                  `json:"count"`
	ModifyCount  int64                  `json:"modify_count"`
	Partitions   map[string]*JSONTable  `json:"partitions"`
}

type jsonExtendedStats struct {
	StatsName  string  `json:"stats_name"`
	DB         string  `json:"db"`
	ColIDs     []int64 `json:"cols"`
	Tp         uint8   `json:"type"`
	ScalarVals float64 `json:"scalar_vals"`
	StringVals string  `json:"string_vals"`
}

func dumpJSONExtendedStats(statsColl *statistics.ExtendedStatsColl) []*jsonExtendedStats {
	if statsColl == nil || len(statsColl.Stats) == 0 {
		return nil
	}
	stats := make([]*jsonExtendedStats, 0, len(statsColl.Stats))
	for key, item := range statsColl.Stats {
		js := &jsonExtendedStats{
			StatsName:  key.StatsName,
			DB:         key.DB,
			ColIDs:     item.ColIDs,
			Tp:         item.Tp,
			ScalarVals: item.ScalarVals,
			StringVals: item.StringVals,
		}
		stats = append(stats, js)
	}
	return stats
}

func extendedStatsFromJSON(statsColl []*jsonExtendedStats) *statistics.ExtendedStatsColl {
	if len(statsColl) == 0 {
		return nil
	}
	stats := statistics.NewExtendedStatsColl()
	for _, js := range statsColl {
		key := statistics.ExtendedStatsKey{
			StatsName: js.StatsName,
			DB:        js.DB,
		}
		item := &statistics.ExtendedStatsItem{
			ColIDs:     js.ColIDs,
			Tp:         js.Tp,
			ScalarVals: js.ScalarVals,
			StringVals: js.StringVals,
		}
		stats.Stats[key] = item
	}
	return stats
}

type jsonColumn struct {
	Histogram         *tipb.Histogram `json:"histogram"`
	CMSketch          *tipb.CMSketch  `json:"cm_sketch"`
	NullCount         int64           `json:"null_count"`
	TotColSize        int64           `json:"tot_col_size"`
	LastUpdateVersion uint64          `json:"last_update_version"`
	Correlation       float64         `json:"correlation"`
	// StatsVer is a pointer here since the old version json file would not contain version information.
	StatsVer *int64 `json:"stats_ver"`
}

func dumpJSONCol(hist *statistics.Histogram, CMSketch *statistics.CMSketch, topn *statistics.TopN, statsVer *int64) *jsonColumn {
	jsonCol := &jsonColumn{
		Histogram:         statistics.HistogramToProto(hist),
		NullCount:         hist.NullCount,
		TotColSize:        hist.TotColSize,
		LastUpdateVersion: hist.LastUpdateVersion,
		Correlation:       hist.Correlation,
		StatsVer:          statsVer,
	}
	if CMSketch != nil || topn != nil {
		jsonCol.CMSketch = statistics.CMSketchToProto(CMSketch, topn)
	}
	return jsonCol
}

// DumpStatsToJSON dumps statistic to json.
func (h *Handle) DumpStatsToJSON(dbName string, tableInfo *model.TableInfo, historyStatsExec sqlexec.RestrictedSQLExecutor) (*JSONTable, error) {
	pi := tableInfo.GetPartitionInfo()
	if pi == nil || h.CurrentPruneMode() == variable.DynamicOnly {
		return h.tableStatsToJSON(dbName, tableInfo, tableInfo.ID, historyStatsExec)
	}
	jsonTbl := &JSONTable{
		DatabaseName: dbName,
		TableName:    tableInfo.Name.L,
		Partitions:   make(map[string]*JSONTable, len(pi.Definitions)),
	}
	for _, def := range pi.Definitions {
		tbl, err := h.tableStatsToJSON(dbName, tableInfo, def.ID, historyStatsExec)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if tbl == nil {
			continue
		}
		jsonTbl.Partitions[def.Name.L] = tbl
	}
	return jsonTbl, nil
}

func (h *Handle) tableStatsToJSON(dbName string, tableInfo *model.TableInfo, physicalID int64, historyStatsExec sqlexec.RestrictedSQLExecutor) (*JSONTable, error) {
	tbl, err := h.tableStatsFromStorage(tableInfo, physicalID, true, historyStatsExec)
	if err != nil || tbl == nil {
		return nil, err
	}
	tbl.Version, tbl.ModifyCount, tbl.Count, err = h.statsMetaByTableIDFromStorage(physicalID, historyStatsExec)
	if err != nil {
		return nil, err
	}
	jsonTbl := &JSONTable{
		DatabaseName: dbName,
		TableName:    tableInfo.Name.L,
		Columns:      make(map[string]*jsonColumn, len(tbl.Columns)),
		Indices:      make(map[string]*jsonColumn, len(tbl.Indices)),
		Count:        tbl.Count,
		ModifyCount:  tbl.ModifyCount,
	}

	for _, col := range tbl.Columns {
		sc := &stmtctx.StatementContext{TimeZone: time.UTC}
		hist, err := col.ConvertTo(sc, types.NewFieldType(mysql.TypeBlob))
		if err != nil {
			return nil, errors.Trace(err)
		}
		jsonTbl.Columns[col.Info.Name.L] = dumpJSONCol(hist, col.CMSketch, col.TopN, &col.StatsVer)
	}

	for _, idx := range tbl.Indices {
		jsonTbl.Indices[idx.Info.Name.L] = dumpJSONCol(&idx.Histogram, idx.CMSketch, idx.TopN, &idx.StatsVer)
	}
	jsonTbl.ExtStats = dumpJSONExtendedStats(tbl.ExtendedStats)
	return jsonTbl, nil
}

// LoadStatsFromJSON will load statistic from JSONTable, and save it to the storage.
func (h *Handle) LoadStatsFromJSON(is infoschema.InfoSchema, jsonTbl *JSONTable) error {
	table, err := is.TableByName(model.NewCIStr(jsonTbl.DatabaseName), model.NewCIStr(jsonTbl.TableName))
	if err != nil {
		return errors.Trace(err)
	}
	tableInfo := table.Meta()
	pi := tableInfo.GetPartitionInfo()
	if pi == nil || jsonTbl.Partitions == nil {
		err := h.loadStatsFromJSON(tableInfo, tableInfo.ID, jsonTbl)
		if err != nil {
			return errors.Trace(err)
		}
	} else {
		for _, def := range pi.Definitions {
			tbl := jsonTbl.Partitions[def.Name.L]
			if tbl == nil {
				continue
			}
			err := h.loadStatsFromJSON(tableInfo, def.ID, tbl)
			if err != nil {
				return errors.Trace(err)
			}
		}
	}
	return errors.Trace(h.Update(is))
}

func (h *Handle) loadStatsFromJSON(tableInfo *model.TableInfo, physicalID int64, jsonTbl *JSONTable) error {
	tbl, err := TableStatsFromJSON(tableInfo, physicalID, jsonTbl)
	if err != nil {
		return errors.Trace(err)
	}

	for _, col := range tbl.Columns {
		err = h.SaveStatsToStorage(tbl.PhysicalID, tbl.Count, 0, &col.Histogram, col.CMSketch, col.TopN, int(col.StatsVer), 1)
		if err != nil {
			return errors.Trace(err)
		}
	}
	for _, idx := range tbl.Indices {
		err = h.SaveStatsToStorage(tbl.PhysicalID, tbl.Count, 1, &idx.Histogram, idx.CMSketch, idx.TopN, int(idx.StatsVer), 1)
		if err != nil {
			return errors.Trace(err)
		}
	}
	err = h.SaveExtendedStatsToStorage(tbl.PhysicalID, tbl.ExtendedStats, true)
	if err != nil {
		return errors.Trace(err)
	}
	return h.SaveMetaToStorage(tbl.PhysicalID, tbl.Count, tbl.ModifyCount)
}

// TableStatsFromJSON loads statistic from JSONTable and return the Table of statistic.
func TableStatsFromJSON(tableInfo *model.TableInfo, physicalID int64, jsonTbl *JSONTable) (*statistics.Table, error) {
	newHistColl := statistics.HistColl{
		PhysicalID:     physicalID,
		HavePhysicalID: true,
		Count:          jsonTbl.Count,
		ModifyCount:    jsonTbl.ModifyCount,
		Columns:        make(map[int64]*statistics.Column, len(jsonTbl.Columns)),
		Indices:        make(map[int64]*statistics.Index, len(jsonTbl.Indices)),
	}
	tbl := &statistics.Table{
		HistColl: newHistColl,
	}
	for id, jsonIdx := range jsonTbl.Indices {
		for _, idxInfo := range tableInfo.Indices {
			if idxInfo.Name.L != id {
				continue
			}
			hist := statistics.HistogramFromProto(jsonIdx.Histogram)
			hist.ID, hist.NullCount, hist.LastUpdateVersion, hist.Correlation = idxInfo.ID, jsonIdx.NullCount, jsonIdx.LastUpdateVersion, jsonIdx.Correlation
			cm, topN := statistics.CMSketchAndTopNFromProto(jsonIdx.CMSketch)
			// If the statistics is loaded from a JSON without stats version,
			// we set it to 1.
			statsVer := int64(statistics.Version1)
			if jsonIdx.StatsVer != nil {
				statsVer = *jsonIdx.StatsVer
			}
			idx := &statistics.Index{
				Histogram: *hist,
				CMSketch:  cm,
				TopN:      topN,
				Info:      idxInfo,
				StatsVer:  statsVer,
			}
			tbl.Indices[idx.ID] = idx
		}
	}

	for id, jsonCol := range jsonTbl.Columns {
		for _, colInfo := range tableInfo.Columns {
			if colInfo.Name.L != id {
				continue
			}
			hist := statistics.HistogramFromProto(jsonCol.Histogram)
			count := int64(hist.TotalRowCount())
			sc := &stmtctx.StatementContext{TimeZone: time.UTC}
			hist, err := hist.ConvertTo(sc, &colInfo.FieldType)
			if err != nil {
				return nil, errors.Trace(err)
			}
			cm, topN := statistics.CMSketchAndTopNFromProto(jsonCol.CMSketch)
			hist.ID, hist.NullCount, hist.LastUpdateVersion, hist.TotColSize, hist.Correlation = colInfo.ID, jsonCol.NullCount, jsonCol.LastUpdateVersion, jsonCol.TotColSize, jsonCol.Correlation
			// If the statistics is loaded from a JSON without stats version,
			// we set it to 1.
			statsVer := int64(statistics.Version1)
			if jsonCol.StatsVer != nil {
				statsVer = *jsonCol.StatsVer
			}
			col := &statistics.Column{
				PhysicalID: physicalID,
				Histogram:  *hist,
				CMSketch:   cm,
				TopN:       topN,
				Info:       colInfo,
				Count:      count,
				IsHandle:   tableInfo.PKIsHandle && mysql.HasPriKeyFlag(colInfo.Flag),
				StatsVer:   statsVer,
			}
			tbl.Columns[col.ID] = col
		}
	}
	tbl.ExtendedStats = extendedStatsFromJSON(jsonTbl.ExtStats)
	return tbl, nil
}
