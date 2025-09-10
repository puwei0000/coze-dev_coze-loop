// Copyright (c) 2025 coze-dev Authors
// SPDX-License-Identifier: Apache-2.0

package ck

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/coze-dev/coze-loop/backend/infra/ck"
	"github.com/coze-dev/coze-loop/backend/modules/observability/infra/repo/ck/gorm_gen/model"
)

type InsertAnnotationParam struct {
	Table       string
	Annotations []*model.ObservabilityAnnotation
}

type GetAnnotationParam struct {
	Tables    []string
	ID        string
	StartTime int64 // us
	EndTime   int64 // us
	Limit     int32
}

type ListAnnotationsParam struct {
	Tables          []string
	SpanIDs         []string
	StartTime       int64 // us
	EndTime         int64 // us
	DescByUpdatedAt bool
	Limit           int32
}

//go:generate mockgen -destination=mocks/annotation_dao.go -package=mocks . IAnnotationDao
type IAnnotationDao interface {
	Insert(context.Context, *InsertAnnotationParam) error
	Get(context.Context, *GetAnnotationParam) (*model.ObservabilityAnnotation, error)
	List(context.Context, *ListAnnotationsParam) ([]*model.ObservabilityAnnotation, error)
}

func NewAnnotationCkDaoImpl(db ck.Provider) (IAnnotationDao, error) {
	return &AnnotationCkDaoImpl{
		db: db,
	}, nil
}

type AnnotationCkDaoImpl struct {
	db ck.Provider
}

func (a *AnnotationCkDaoImpl) Insert(ctx context.Context, params *InsertAnnotationParam) error {
	if params == nil || len(params.Annotations) == 0 {
		return nil
	}
	
	db := a.db.NewSession(ctx)
	retryTimes := 3
	var lastErr error
	
	for i := 0; i < retryTimes; i++ {
		if err := db.Table(params.Table).Create(params.Annotations).Error; err != nil {
			lastErr = err
		} else {
			return nil
		}
	}
	return lastErr
}

func (a *AnnotationCkDaoImpl) Get(ctx context.Context, params *GetAnnotationParam) (*model.ObservabilityAnnotation, error) {
	if params == nil || params.ID == "" {
		return nil, nil
	}
	
	db, tableName, err := a.buildSql(ctx, &annoSqlParam{
		Tables:    params.Tables,
		StartTime: params.StartTime,
		EndTime:   params.EndTime,
		ID:        params.ID,
		Limit:     1,
	})
	if err != nil {
		return nil, err
	}
	
	var annotations []*model.ObservabilityAnnotation
	if tableName != "" {
		// Single table query with FINAL for consistency
		if err := db.Table(tableName+" FINAL").Find(&annotations).Error; err != nil {
			return nil, err
		}
	} else {
		// Multi-table union query
		if err := db.Find(&annotations).Error; err != nil {
			return nil, err
		}
	}
	
	if len(annotations) == 0 {
		return nil, nil
	}
	return annotations[0], nil
}

func (a *AnnotationCkDaoImpl) List(ctx context.Context, params *ListAnnotationsParam) ([]*model.ObservabilityAnnotation, error) {
	if params == nil || len(params.SpanIDs) == 0 {
		return nil, nil
	}
	
	db, _, err := a.buildSql(ctx, &annoSqlParam{
		Tables:          params.Tables,
		StartTime:       params.StartTime,
		EndTime:         params.EndTime,
		SpanIDs:         params.SpanIDs,
		DescByUpdatedAt: params.DescByUpdatedAt,
		Limit:           params.Limit,
	})
	if err != nil {
		return nil, err
	}
	
	var annotations []*model.ObservabilityAnnotation
	if err := db.Find(&annotations).Error; err != nil {
		return nil, err
	}
	
	return annotations, nil
}

// annoSqlParam 内部SQL构建参数
type annoSqlParam struct {
	Tables          []string
	StartTime       int64
	EndTime         int64
	ID              string
	SpanIDs         []string
	DescByUpdatedAt bool
	Limit           int32
}

// buildSql 构建SQL查询，返回(query, singleTableName, error)
func (a *AnnotationCkDaoImpl) buildSql(ctx context.Context, param *annoSqlParam) (*gorm.DB, string, error) {
	db := a.db.NewSession(ctx)
	var tableQueries []*gorm.DB
	
	for _, table := range param.Tables {
		query, err := a.buildSingleSql(ctx, db, table, param)
		if err != nil {
			return nil, "", err
		}
		tableQueries = append(tableQueries, query)
	}
	
	if len(tableQueries) == 0 {
		return nil, "", fmt.Errorf("no table configured")
	} else if len(tableQueries) == 1 {
		// 单表查询，返回表名以便使用FINAL
		return tableQueries[0], param.Tables[0], nil
	} else {
		// 多表联合查询
		queries := make([]string, 0)
		for i := 0; i < len(tableQueries); i++ {
			query := tableQueries[i].ToSQL(func(tx *gorm.DB) *gorm.DB {
				return tx.Find(nil)
			})
			queries = append(queries, "("+query+")")
		}
		sql := fmt.Sprintf("SELECT * FROM (%s)", strings.Join(queries, " UNION ALL "))
		if param.DescByUpdatedAt {
			sql += " ORDER BY updated_at DESC"
		}
		if param.Limit > 0 {
			sql += fmt.Sprintf(" LIMIT %d", param.Limit)
		}
		return db.Raw(sql), "", nil
	}
}

// buildSingleSql 构建单表查询SQL
func (a *AnnotationCkDaoImpl) buildSingleSql(ctx context.Context, db *gorm.DB, tableName string, param *annoSqlParam) (*gorm.DB, error) {
	sqlQuery := db.Table(tableName)
	
	// 时间范围过滤
	if param.StartTime > 0 {
		sqlQuery = sqlQuery.Where("start_time >= ?", param.StartTime)
	}
	if param.EndTime > 0 {
		sqlQuery = sqlQuery.Where("start_time <= ?", param.EndTime)
	}
	
	// ID过滤
	if param.ID != "" {
		sqlQuery = sqlQuery.Where("id = ?", param.ID)
	}
	
	// SpanIDs过滤
	if len(param.SpanIDs) > 0 {
		sqlQuery = sqlQuery.Where("span_id IN (?)", param.SpanIDs)
	}
	
	// 排序
	if param.DescByUpdatedAt {
		sqlQuery = sqlQuery.Order(clause.OrderBy{Columns: []clause.OrderByColumn{
			{Column: clause.Column{Name: "updated_at"}, Desc: true},
		}})
	}
	
	// 限制
	if param.Limit > 0 {
		sqlQuery = sqlQuery.Limit(int(param.Limit))
	}
	
	return sqlQuery, nil
}

// convertIntoPartitions 将时间范围转换为分区列表
func convertIntoPartitions(startAt, endAt int64) []string {
	if startAt <= 0 || endAt <= 0 || startAt > endAt {
		return nil
	}
	
	startTime := time.UnixMicro(startAt)
	endTime := time.UnixMicro(endAt)
	
	var partitions []string
	current := startTime
	for current.Before(endTime) || current.Equal(endTime) {
		partition := current.Format("2006-01-02")
		partitions = append(partitions, partition)
		current = current.AddDate(0, 0, 1)
	}
	
	return partitions
}