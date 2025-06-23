package apis

import (
	cryptoRand "crypto/rand"
	"database/sql"
	"errors"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/denisenkom/go-mssqldb"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/list"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/search"
	"github.com/pocketbase/pocketbase/tools/security"
)

// bindRecordCrudApi registers the record crud api endpoints and
// the corresponding handlers.
//
// note: the rate limiter is "inlined" because some of the crud actions are also used in the batch APIs
func bindRecordCrudApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	subGroup := rg.Group("/collections/{collection}/records").Unbind(DefaultRateLimitMiddlewareId)
	subGroup.GET("", recordsList)
	subGroup.GET("/{id}", recordView)
	subGroup.POST("", recordCreate(true, nil)).Bind(dynamicCollectionBodyLimit(""))
	subGroup.PATCH("/{id}", recordUpdate(true, nil)).Bind(dynamicCollectionBodyLimit(""))
	subGroup.DELETE("/{id}", recordDelete(true, nil))
}

func recordsList(e *core.RequestEvent) error {
	collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
	if err != nil || collection == nil {
		return e.NotFoundError("Missing collection context.", err)
	}

	// Connect to SQL Server
	connString := os.Getenv("SQLSERVER_CONN_STRING")
	sqlDB, err := sql.Open("sqlserver", connString)
	if err != nil {
		return e.InternalServerError("Failed to connect to SQL Server: "+err.Error(), nil)
	}
	defer sqlDB.Close()

	// Build SELECT query for all fields
	fieldNames := []string{}
	for _, field := range collection.Fields {
		fieldNames = append(fieldNames, "["+field.GetName()+"]")
	}
	// Always include id if present
	fieldNames = append([]string{"[id]"}, fieldNames...)
	queryStr := "SELECT " + strings.Join(fieldNames, ", ") + " FROM [" + collection.Name + "]"

	rows, err := sqlDB.Query(queryStr)
	if err != nil {
		return e.InternalServerError("Failed to fetch records from SQL Server: "+err.Error(), nil)
	}
	defer rows.Close()

	// Prepare result slice
	var results []map[string]interface{}

	cols, err := rows.Columns()
	if err != nil {
		return e.InternalServerError("Failed to get columns: "+err.Error(), nil)
	}

	for rows.Next() {
		// Prepare a slice of pointers for Scan
		values := make([]interface{}, len(cols))
		valuePtrs := make([]interface{}, len(cols))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return e.InternalServerError("Failed to scan row: "+err.Error(), nil)
		}

		rowMap := make(map[string]interface{})
		for i, col := range cols {
			val := values[i]
			// Convert []byte to string for text fields
			if b, ok := val.([]byte); ok {
				rowMap[col] = string(b)
			} else {
				rowMap[col] = val
			}
		}
		results = append(results, rowMap)
	}

	// Wrap in search.Result for consistent API response
	result := &search.Result{
		Items:      results,
		Page:       1,
		PerPage:    len(results),
		TotalItems: len(results),
		TotalPages: 1,
	}

	return e.JSON(http.StatusOK, result)
}

var listTimingRateLimitRule = core.RateLimitRule{MaxRequests: 3, Duration: 3}

func randomizedThrottle(softMax int64) {
	var timeout int64
	randRange, err := cryptoRand.Int(cryptoRand.Reader, big.NewInt(softMax))
	if err == nil {
		timeout = randRange.Int64()
	} else {
		timeout = softMax
	}

	time.Sleep(time.Duration(timeout) * time.Millisecond)
}

func recordView(e *core.RequestEvent) error {
	collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
	if err != nil || collection == nil {
		return e.NotFoundError("Missing collection context.", err)
	}

	recordId := e.Request.PathValue("id")
	if recordId == "" {
		return e.NotFoundError("", nil)
	}

	// Connect to SQL Server
	connString := os.Getenv("SQLSERVER_CONN_STRING")
	sqlDB, err := sql.Open("sqlserver", connString)
	if err != nil {
		return e.InternalServerError("Failed to connect to SQL Server: "+err.Error(), nil)
	}
	defer sqlDB.Close()

	// Build SELECT query for all fields
	fieldNames := []string{}
	for _, field := range collection.Fields {
		fieldNames = append(fieldNames, "["+field.GetName()+"]")
	}
	// Always include id if present
	fieldNames = append([]string{"[id]"}, fieldNames...)
	queryStr := "SELECT " + strings.Join(fieldNames, ", ") + " FROM [" + collection.Name + "] WHERE [id]=@p1"

	row := sqlDB.QueryRow(queryStr, recordId)

	cols := fieldNames
	values := make([]interface{}, len(cols))
	valuePtrs := make([]interface{}, len(cols))
	for i := range values {
		valuePtrs[i] = &values[i]
	}

	if err := row.Scan(valuePtrs...); err != nil {
		if err == sql.ErrNoRows {
			return e.NotFoundError("Record not found.", nil)
		}
		return e.InternalServerError("Failed to scan record: "+err.Error(), nil)
	}

	// Populate a core.Record
	record := core.NewRecord(collection)
	for i, col := range cols {
		val := values[i]
		if b, ok := val.([]byte); ok {
			record.Set(strings.Trim(col, "[]"), string(b))
		} else {
			record.Set(strings.Trim(col, "[]"), val)
		}
	}
	// Set the record ID if present
	if idVal, ok := record.Get("id").(string); ok {
		record.Id = idVal
	}

	return e.JSON(http.StatusOK, record)
}

func recordCreate(responseWriteAfterTx bool, optFinalizer func(data any) error) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
		if err != nil || collection == nil {
			return e.NotFoundError("Missing collection context.", err)
		}

		if collection.IsView() {
			return e.BadRequestError("Unsupported collection type.", nil)
		}

		// Read request data
		record := core.NewRecord(collection)
		data, err := recordDataFromRequest(e, record)
		if err != nil {
			return firstApiError(err, e.BadRequestError("Failed to read the submitted data.", err))
		}

		connString := os.Getenv("SQLSERVER_CONN_STRING")
		sqlDB, err := sql.Open("sqlserver", connString)
		if err != nil {
			return e.InternalServerError("Failed to connect to SQL Server: "+err.Error(), nil)
		}
		defer sqlDB.Close()

		fieldNames := []string{}
		placeholders := []string{}
		values := []interface{}{}

		id := data["id"]
		if id == nil || id == "" {
			id = security.PseudorandomString(15)
		}
		fieldNames = append(fieldNames, "[id]")
		placeholders = append(placeholders, "@p_id")
		values = append(values, id)

		now := time.Now().UTC()
		created := data["created"]
		if created == nil || created == "" {
			created = now
		}
		updated := data["updated"]
		if updated == nil || updated == "" {
			updated = now
		}
		fieldNames = append(fieldNames, "[created]", "[updated]")
		placeholders = append(placeholders, "@p_created", "@p_updated")
		values = append(values, created, updated)

		for _, field := range collection.Fields {
			name := field.GetName()
			if val, ok := data[name]; ok {
				fieldNames = append(fieldNames, "["+name+"]")
				placeholders = append(placeholders, "@p_"+name)
				values = append(values, val)
			}
		}

		insertSQL := "INSERT INTO [" + collection.Name + "] (" + strings.Join(fieldNames, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ")"
		stmt, err := sqlDB.Prepare(insertSQL)
		if err != nil {
			return e.InternalServerError("Failed to prepare insert statement: "+err.Error(), nil)
		}
		defer stmt.Close()

		// Build named arguments for SQL Server
		args := []interface{}{}
		for i, p := range placeholders {
			args = append(args, sql.Named(strings.TrimPrefix(p, "@"), values[i]))
		}

		_, err = stmt.Exec(args...)
		if err != nil {
			return e.InternalServerError("Failed to insert record into SQL Server: "+err.Error(), nil)
		}

		// Fetch the inserted record to return (including created/updated)
		selectFields := append([]string{"[id]", "[created]", "[updated]"}, func() []string {
			names := []string{}
			for _, field := range collection.Fields {
				names = append(names, "["+field.GetName()+"]")
			}
			return names
		}()...)
		selectSQL := "SELECT " + strings.Join(selectFields, ", ") + " FROM [" + collection.Name + "] WHERE [id]=@p_id"
		row := sqlDB.QueryRow(selectSQL, sql.Named("p_id", id))

		cols := selectFields
		scanVals := make([]interface{}, len(cols))
		scanPtrs := make([]interface{}, len(cols))
		for i := range scanVals {
			scanPtrs[i] = &scanVals[i]
		}

		if err := row.Scan(scanPtrs...); err != nil {
			return e.InternalServerError("Failed to fetch inserted record: "+err.Error(), nil)
		}

		record = core.NewRecord(collection)
		for i, col := range cols {
			val := scanVals[i]
			if b, ok := val.([]byte); ok {
				record.Set(strings.Trim(col, "[]"), string(b))
			} else {
				record.Set(strings.Trim(col, "[]"), val)
			}
		}
		if idVal, ok := record.Get("id").(string); ok {
			record.Id = idVal
		}

		return e.JSON(http.StatusOK, record)
	}
}

func recordUpdate(responseWriteAfterTx bool, optFinalizer func(data any) error) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
		if err != nil || collection == nil {
			return e.NotFoundError("Missing collection context.", err)
		}

		if collection.IsView() {
			return e.BadRequestError("Unsupported collection type.", nil)
		}

		err = checkCollectionRateLimit(e, collection, "update")
		if err != nil {
			return err
		}

		recordId := e.Request.PathValue("id")
		if recordId == "" {
			return e.NotFoundError("", nil)
		}

		requestInfo, err := e.RequestInfo()
		if err != nil {
			return firstApiError(err, e.BadRequestError("", err))
		}

		hasSuperuserAuth := requestInfo.HasSuperuserAuth()
		if !hasSuperuserAuth && collection.UpdateRule == nil {
			return firstApiError(err, e.ForbiddenError("Only superusers can perform this action.", nil))
		}

		// Read request data
		record := core.NewRecord(collection)
		data, err := recordDataFromRequest(e, record)
		if err != nil {
			return firstApiError(err, e.BadRequestError("Failed to read the submitted data.", err))
		}

		// Connect to SQL Server
		connString := os.Getenv("SQLSERVER_CONN_STRING")
		sqlDB, err := sql.Open("sqlserver", connString)
		if err != nil {
			return e.InternalServerError("Failed to connect to SQL Server: "+err.Error(), nil)
		}
		defer sqlDB.Close()

		// Prepare update statement
		setClauses := []string{}
		args := []interface{}{}

		// Always update updated timestamp
		now := time.Now().UTC()
		updated := data["updated"]
		if updated == nil || updated == "" {
			updated = now
		}
		setClauses = append(setClauses, "[updated]=@p_updated")
		args = append(args, sql.Named("p_updated", updated))

		for _, field := range collection.Fields {
			name := field.GetName()
			if name == "id" {
				continue // skip id field for update
			}
			if val, ok := data[name]; ok {
				setClauses = append(setClauses, "["+name+"]=@p_"+name)
				args = append(args, sql.Named("p_"+name, val))
			}
		}

		args = append(args, sql.Named("p_id", recordId))

		updateSQL := "UPDATE [" + collection.Name + "] SET " + strings.Join(setClauses, ", ") + " WHERE [id]=@p_id"

		_, err = sqlDB.Exec(updateSQL, args...)
		if err != nil {
			return e.InternalServerError("Failed to update record in SQL Server: "+err.Error(), nil)
		}

		// Fetch the updated record to return (including created/updated)
		selectFields := append([]string{"[id]", "[created]", "[updated]"}, func() []string {
			names := []string{}
			for _, field := range collection.Fields {
				names = append(names, "["+field.GetName()+"]")
			}
			return names
		}()...)
		selectSQL := "SELECT " + strings.Join(selectFields, ", ") + " FROM [" + collection.Name + "] WHERE [id]=@p_id"
		row := sqlDB.QueryRow(selectSQL, sql.Named("p_id", recordId))

		cols := selectFields
		scanVals := make([]interface{}, len(cols))
		scanPtrs := make([]interface{}, len(cols))
		for i := range scanVals {
			scanPtrs[i] = &scanVals[i]
		}

		if err := row.Scan(scanPtrs...); err != nil {
			return e.InternalServerError("Failed to fetch updated record: "+err.Error(), nil)
		}

		record = core.NewRecord(collection)
		for i, col := range cols {
			val := scanVals[i]
			if b, ok := val.([]byte); ok {
				record.Set(strings.Trim(col, "[]"), string(b))
			} else {
				record.Set(strings.Trim(col, "[]"), val)
			}
		}
		if idVal, ok := record.Get("id").(string); ok {
			record.Id = idVal
		}

		return e.JSON(http.StatusOK, record)
	}
}

func recordDelete(responseWriteAfterTx bool, optFinalizer func(data any) error) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
		if err != nil || collection == nil {
			return e.NotFoundError("Missing collection context.", err)
		}

		if collection.IsView() {
			return e.BadRequestError("Unsupported collection type.", nil)
		}

		err = checkCollectionRateLimit(e, collection, "delete")
		if err != nil {
			return err
		}

		recordId := e.Request.PathValue("id")
		if recordId == "" {
			return e.NotFoundError("", nil)
		}

		requestInfo, err := e.RequestInfo()
		if err != nil {
			return firstApiError(err, e.BadRequestError("", err))
		}

		if !requestInfo.HasSuperuserAuth() && collection.DeleteRule == nil {
			return e.ForbiddenError("Only superusers can perform this action.", nil)
		}

		// Connect to SQL Server
		connString := os.Getenv("SQLSERVER_CONN_STRING")
		sqlDB, err := sql.Open("sqlserver", connString)
		if err != nil {
			return e.InternalServerError("Failed to connect to SQL Server: "+err.Error(), nil)
		}
		defer sqlDB.Close()

		// Optionally: check if record exists before deleting
		var exists int
		checkSQL := "SELECT COUNT(1) FROM [" + collection.Name + "] WHERE [id]=@p_id"
		err = sqlDB.QueryRow(checkSQL, sql.Named("p_id", recordId)).Scan(&exists)
		if err != nil {
			return e.InternalServerError("Failed to check record existence: "+err.Error(), nil)
		}
		if exists == 0 {
			return e.NotFoundError("Record not found.", nil)
		}

		// Delete the record
		deleteSQL := "DELETE FROM [" + collection.Name + "] WHERE [id]=@p_id"
		_, err = sqlDB.Exec(deleteSQL, sql.Named("p_id", recordId))
		if err != nil {
			return e.InternalServerError("Failed to delete record from SQL Server: "+err.Error(), nil)
		}

		// Optionally call optFinalizer if provided
		if optFinalizer != nil {
		}

		return e.NoContent(http.StatusNoContent)
	}
}

// -------------------------------------------------------------------

func recordDataFromRequest(e *core.RequestEvent, record *core.Record) (map[string]any, error) {
	info, err := e.RequestInfo()
	if err != nil {
		return nil, err
	}

	// resolve regular fields
	result := record.ReplaceModifiers(info.Body)

	// resolve uploaded files
	uploadedFiles, err := extractUploadedFiles(e, record.Collection(), "")
	if err != nil {
		return nil, err
	}
	if len(uploadedFiles) > 0 {
		for k, files := range uploadedFiles {
			uploaded := make([]any, 0, len(files))

			// if not remove/prepend/append -> merge with the submitted
			// info.Body values to prevent accidental old files deletion
			if info.Body[k] != nil &&
				!strings.HasPrefix(k, "+") &&
				!strings.HasSuffix(k, "+") &&
				!strings.HasSuffix(k, "-") {
				existing := list.ToUniqueStringSlice(info.Body[k])
				for _, name := range existing {
					uploaded = append(uploaded, name)
				}
			}

			for _, file := range files {
				uploaded = append(uploaded, file)
			}

			result[k] = uploaded
		}

		result = record.ReplaceModifiers(result)
	}

	isAuth := record.Collection().IsAuth()

	// unset hidden fields for non-superusers
	if !info.HasSuperuserAuth() {
		for _, f := range record.Collection().Fields {
			if f.GetHidden() {
				// exception for the auth collection "password" field
				if isAuth && f.GetName() == core.FieldNamePassword {
					continue
				}

				delete(result, f.GetName())
			}
		}
	}

	return result, nil
}

func extractUploadedFiles(re *core.RequestEvent, collection *core.Collection, prefix string) (map[string][]*filesystem.File, error) {
	contentType := re.Request.Header.Get("content-type")
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		return nil, nil // not multipart/form-data request
	}

	result := map[string][]*filesystem.File{}

	for _, field := range collection.Fields {
		if field.Type() != core.FieldTypeFile {
			continue
		}

		baseKey := field.GetName()

		keys := []string{
			baseKey,
			// prepend and append modifiers
			"+" + baseKey,
			baseKey + "+",
		}

		for _, k := range keys {
			if prefix != "" {
				k = prefix + "." + k
			}
			files, err := re.FindUploadedFiles(k)
			if err != nil && !errors.Is(err, http.ErrMissingFile) {
				return nil, err
			}
			if len(files) > 0 {
				result[k] = files
			}
		}
	}

	return result, nil
}

// hasAuthManageAccess checks whether the client is allowed to have
// [forms.RecordUpsert] auth management permissions
// (e.g. allowing to change system auth fields without oldPassword).
func hasAuthManageAccess(app core.App, requestInfo *core.RequestInfo, collection *core.Collection, query *dbx.SelectQuery) bool {
	if !collection.IsAuth() {
		return false
	}

	manageRule := collection.ManageRule

	if manageRule == nil || *manageRule == "" {
		return false // only for superusers (manageRule can't be empty)
	}

	if requestInfo == nil || requestInfo.Auth == nil {
		return false // no auth record
	}

	resolver := core.NewRecordFieldResolver(app, collection, requestInfo, true)

	expr, err := search.FilterData(*manageRule).BuildExpr(resolver)
	if err != nil {
		app.Logger().Error("Manage rule build expression error", "error", err, "collectionId", collection.Id)
		return false
	}
	query.AndWhere(expr)

	resolver.UpdateQuery(query)

	var exists int

	err = query.Limit(1).Row(&exists)

	return err == nil && exists > 0
}
