package main

import (
	"fmt"
	"github.com/ngaut/log"
	"strings"
	"sync/atomic"

	_ "github.com/go-sql-driver/mysql"
)

func Map(vs []string, f func(string) string) []string {
	vsm := make([]string, len(vs))
	for i, v := range vs {
		vsm[i] = f(v)
	}
	return vsm
}

/*
 This function chunk-splits the table into files based on the dataLength
 and avgRowLength reported in information_schema.  In future, a region
 based strategy will be used, so this function will likely change
 quite a lot.
*/

func (d *Dumper) prepareDumpTable(schema string, table string, avgRowLength int, dataLength int64, primaryKey string, insertableCols string) {

	if dataLength < d.cfg.FileTargetSize {
		d.TableDumpWg.Add(1)
		df := d.createDumpFile(schema, table, primaryKey, insertableCols, 0, 0) // small table
		go d.dumpTableData(df)
		atomic.AddInt64(&d.TotalFiles, 1)
	} else {

		rowsPerFile := d.discoverRowsPerFile(avgRowLength, d.cfg.FileTargetSize)
		min, max := d.discoverTableMinMax(schema, table, primaryKey)

		for i := min; i < max; i += rowsPerFile {

			start := i
			end := i + rowsPerFile - 1

			if i == min {
				start = 0
			}

			if end > max {
				end = 0
			}

			log.Debugf("Table: %s.%s.  Start: %d End: %d\n", schema, table, start, end)
			d.TableDumpWg.Add(1)
			df := d.createDumpFile(schema, table, primaryKey, insertableCols, start, end)
			go d.dumpTableData(df)
			atomic.AddInt64(&d.TotalFiles, 1)

		}
	}
}

func (d *Dumper) dumpTableData(df dumpFile) {

	defer d.TableDumpWg.Done()

	db := d.newDbConnection()
	defer db.Close()

	rows, err := db.Query(df.sql)
	log.Debug(df.sql)

	if err != nil {
		log.Fatal("Could not retrieve table data: %s", df.schema, df.table)
	}

	cols, _ := rows.Columns()
	types, _ := rows.ColumnTypes()
	colsstr := strings.Join(Map(cols, quoteIdentifier), ",")

	// Result is your slice string.
	rawResult := make([][]byte, len(cols))
	result := make([]string, len(cols))

	dest := make([]interface{}, len(cols)) // A temporary interface{} slice
	for i := range rawResult {
		dest[i] = &rawResult[i] // Put pointers to each string in the interface slice
	}

	for rows.Next() {
		err = rows.Scan(dest...)
		if err != nil {
			fmt.Println("Failed to scan row", err)
			return
		}

		for i, raw := range rawResult {
			if raw == nil {
				result[i] = "NULL"
			} else {

				t := types[i].DatabaseTypeName()

				// TODO: are there more numeric types?
				if t == "BIGINT" || t == "INT" || t == "DECIMAL" || t == "FLOAT" {
					result[i] = string(raw)
				} else {
					result[i] = fmt.Sprintf("'%s'", quoteString(string(raw)))
				}
			}
		}

		values := fmt.Sprintf("(%s)", strings.Join(result, ","))

		if int64(df.bufferLen()+len(values)) > d.cfg.BulkInsertLimit {
			df.write(";\n")
			df.flush()
		}

		if df.bufferLen() == 0 {
			df.write(fmt.Sprintf("INSERT INTO %s (%s) VALUES \n%s", df.table, colsstr, values))
		} else {
			df.write(",\n")
			df.write(values)
		}

	}

	// Flush any remaining buffer

	if df.bufferLen() > 0 {
		df.write(";\n")
		df.flush()
	}

	df.close()
	atomic.AddInt64(&d.FilesDumpCompleted, 1)
	d.TableCopyWg.Add(1)
	go d.copyFileToS3(df.file, "table")

}
