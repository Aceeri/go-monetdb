package monetdb

import (
	"bytes"
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"

	"monetdb/mapi"
)

type Stmt struct {
	conn  *Conn
	query string

	data *StmtData
}

type StmtData struct {
	execId int

	lastRowId   int64
	rowCount    int64
	queryId     int64
	offset      int64
	columnCount int64

	rows        [][]driver.Value
	description []description
}

type description struct {
	columnName   string
	columnType   string
	displaySize  int
	internalSize int
	precision    int
	scale        int
	nullOk       int
}

func newStmt(c *Conn, q string) Stmt {
	s := Stmt{
		conn:  c,
		query: q,
		data: &StmtData{
			execId: -1,
		},
	}
	return s
}

func (s Stmt) Close() error {
	s.conn = nil
	s.data = nil
	return nil
}

func (s Stmt) NumInput() int {
	return -1
}

func (s Stmt) Exec(args []driver.Value) (driver.Result, error) {
	res := newResult()

	r, err := s.exec(args)
	if err != nil {
		res.err = err
		return res, res.err
	}

	err = s.storeResult(r)
	res.lastInsertId = s.data.lastRowId
	res.rowsAffected = s.data.rowCount
	res.err = err

	return res, res.err
}

func (s Stmt) Query(args []driver.Value) (driver.Rows, error) {
	rows := newRows(s)

	r, err := s.exec(args)
	if err != nil {
		rows.err = err
		return rows, rows.err
	}

	err = s.storeResult(r)
	rows.queryId = s.data.queryId
	rows.data.lastRowId = s.data.lastRowId
	rows.data.rowCount = s.data.rowCount
	rows.data.offset = s.data.offset
	rows.data.rows = s.data.rows
	rows.data.description = s.data.description

	return rows, rows.err
}

func (s Stmt) exec(args []driver.Value) (string, error) {
	if s.data.execId == -1 {
		err := s.prepareQuery()
		if err != nil {
			return "", err
		}
	}

	var b bytes.Buffer
	b.WriteString(fmt.Sprintf("EXEC %d (", s.data.execId))

	for i, v := range args {
		str, err := convertToMonet(v)
		if err != nil {
			return "", nil
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(str)
	}

	b.WriteString(")")
	return s.conn.execute(b.String())
}

func (s Stmt) prepareQuery() error {
	q := fmt.Sprintf("PREPARE %s", s.query)
	r, err := s.conn.execute(q)
	if err != nil {
		return err
	}

	return s.storeResult(r)
}

func (s Stmt) storeResult(r string) error {
	var columnNames []string
	var columnTypes []string
	var displaySizes []int
	var internalSizes []int
	var precisions []int
	var scales []int
	var nullOks []int

	for _, line := range strings.Split(r, "\n") {
		if strings.HasPrefix(line, mapi.MSG_INFO) {
			// TODO log

		} else if strings.HasPrefix(line, mapi.MSG_QPREPARE) {
			t := strings.Split(strings.TrimSpace(line[2:]), " ")
			s.data.execId, _ = strconv.Atoi(t[0])
			return nil

		} else if strings.HasPrefix(line, mapi.MSG_QTABLE) {
			t := strings.Split(strings.TrimSpace(line[2:]), " ")
			s.data.queryId, _ = strconv.ParseInt(t[0], 10, 64)
			s.data.rowCount, _ = strconv.ParseInt(t[1], 10, 64)
			s.data.columnCount, _ = strconv.ParseInt(t[2], 10, 64)

			columnNames = make([]string, s.data.columnCount)
			columnTypes = make([]string, s.data.columnCount)
			displaySizes = make([]int, s.data.columnCount)
			internalSizes = make([]int, s.data.columnCount)
			precisions = make([]int, s.data.columnCount)
			scales = make([]int, s.data.columnCount)
			nullOks = make([]int, s.data.columnCount)

		} else if strings.HasPrefix(line, mapi.MSG_TUPLE) {
			v, err := s.parseTuple(line)
			if err != nil {
				return err
			}
			s.data.rows = append(s.data.rows, v)

		} else if strings.HasPrefix(line, mapi.MSG_QBLOCK) {
			s.data.rows = make([][]driver.Value, 0)

		} else if strings.HasPrefix(line, mapi.MSG_QSCHEMA) {
			s.data.offset = 0
			s.data.rows = make([][]driver.Value, 0)
			s.data.lastRowId = 0
			s.data.description = nil
			s.data.rowCount = 0

		} else if strings.HasPrefix(line, mapi.MSG_QUPDATE) {
			t := strings.Split(strings.TrimSpace(line[2:]), " ")
			c, _ := strconv.ParseInt(t[0], 10, 64)
			i, _ := strconv.ParseInt(t[1], 10, 64)
			s.data.rowCount = c
			s.data.lastRowId = i

		} else if strings.HasPrefix(line, mapi.MSG_QTRANS) {
			s.data.offset = 0
			s.data.rows = make([][]driver.Value, 0, 0)
			s.data.lastRowId = 0
			s.data.description = nil
			s.data.rowCount = 0

		} else if strings.HasPrefix(line, mapi.MSG_HEADER) {
			t := strings.Split(line[1:], "#")
			data := strings.TrimSpace(t[0])
			identity := strings.TrimSpace(t[1])

			values := make([]string, 0)
			for _, value := range strings.Split(data, ",") {
				values = append(values, strings.TrimSpace(value))
			}

			if identity == "name" {
				columnNames = values

			} else if identity == "type" {
				columnTypes = values

			} else if identity == "typesizes" {
				sizes := make([][]int, len(values))
				for i, value := range values {
					s := make([]int, 0)
					for _, v := range strings.Split(value, " ") {
						val, _ := strconv.ParseInt(v, 10, 32)
						s = append(s, int(val))
					}
					internalSizes[i] = s[0]
					sizes = append(sizes, s)
				}
				for j, t := range columnTypes {
					if t == "decimal" {
						precisions[j] = sizes[j][0]
						scales[j] = sizes[j][1]
					}
				}
			}

			s.updateDescription(columnNames, columnTypes, displaySizes,
				internalSizes, precisions, scales, nullOks)
			s.data.offset = 0
			s.data.lastRowId = 0

		} else if strings.HasPrefix(line, mapi.MSG_PROMPT) {
			return nil

		} else if strings.HasPrefix(line, mapi.MSG_ERROR) {
			return fmt.Errorf("error: %s", line[1:])

		}
	}

	return fmt.Errorf("unknown state: %s", r)
}

func (s Stmt) parseTuple(d string) ([]driver.Value, error) {
	items := strings.Split(d[1:len(d)-1], ",\t")
	if len(items) != len(s.data.description) {
		return nil, fmt.Errorf("length of row doesn't match header")
	}

	v := make([]driver.Value, len(items))
	for i, value := range items {
		vv, err := s.convert(value, s.data.description[i].columnType)
		if err != nil {
			return nil, err
		}
		v[i] = vv
	}
	return v, nil
}

func (s Stmt) updateDescription(
	columnNames, columnTypes []string, displaySizes,
	internalSizes, precisions, scales, nullOks []int) {

	d := make([]description, len(columnNames))
	for i, _ := range columnNames {
		desc := description{
			columnName:   columnNames[i],
			columnType:   columnTypes[i],
			displaySize:  displaySizes[i],
			internalSize: internalSizes[i],
			precision:    precisions[i],
			scale:        scales[i],
			nullOk:       nullOks[i],
		}
		d[i] = desc
	}

	s.data.description = d
}

func (s Stmt) convert(value, dataType string) (driver.Value, error) {
	val, err := convertToGo(value, dataType)
	return val, err
}
