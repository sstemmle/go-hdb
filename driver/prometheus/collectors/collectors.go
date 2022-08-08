// SPDX-FileCopyrightText: 2014-2022 SAP SE
//
// SPDX-License-Identifier: Apache-2.0

// Package collectors implements go-hdb prometheus collectors.
package collectors

import (
	"fmt"
	"strings"

	"github.com/SAP/go-hdb/driver"
	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "go_hdb"

type stats interface {
	Stats() driver.Stats
}

type collector struct {
	s stats

	openConnections  *prometheus.Desc
	openTransactions *prometheus.Desc
	openStatements   *prometheus.Desc
	readBytes        *prometheus.Desc
	writtenBytes     *prometheus.Desc
	sqlDurations     *prometheus.Desc
}

func newCollector(s stats, subsystem string, labels prometheus.Labels) prometheus.Collector {
	// fqName: namespace, subsystem, name
	fqName := func(name string) string { return strings.Join([]string{namespace, subsystem, name}, "_") }
	return &collector{
		s: s,
		openConnections: prometheus.NewDesc(
			fqName("open_connections"),
			fmt.Sprintf("The number of established %s connections.", subsystem),
			nil,
			labels,
		),
		openTransactions: prometheus.NewDesc(
			fqName("open_transactions"),
			fmt.Sprintf("The number of open %s transactions.", subsystem),
			nil,
			labels,
		),
		openStatements: prometheus.NewDesc(
			fqName("open_statements"),
			fmt.Sprintf("The number of open %s statements.", subsystem),
			nil,
			labels,
		),
		readBytes: prometheus.NewDesc(
			fqName("bytes_read"),
			fmt.Sprintf("The total bytes read from the database connection of %s statements.", subsystem),
			nil,
			labels,
		),
		writtenBytes: prometheus.NewDesc(
			fqName("bytes_written"),
			fmt.Sprintf("The total bytes written to the database connection of %s statements.", subsystem),
			nil,
			labels,
		),
		sqlDurations: prometheus.NewDesc(
			fqName("sql_duration"),
			fmt.Sprintf("The duration measured in milliseconds for the different sql command categories of %s.", subsystem),
			[]string{"sql"},
			labels,
		),
	}
}

// Describe implements Collector.
func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.openConnections
	ch <- c.openTransactions
	ch <- c.openStatements
	ch <- c.readBytes
	ch <- c.writtenBytes
	for i := 0; i < driver.StatsNumSQL; i++ {
		ch <- c.sqlDurations
	}
}

func durationStatBuckets(s *driver.DurationStat) map[float64]uint64 {
	buckets := map[float64]uint64{}
	for k, v := range s.Buckets {
		buckets[float64(k)] = v
	}
	return buckets
}

// Collect implements Collector.
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	stats := c.s.Stats()
	ch <- prometheus.MustNewConstMetric(c.openConnections, prometheus.GaugeValue, float64(stats.OpenConnections))
	ch <- prometheus.MustNewConstMetric(c.openTransactions, prometheus.GaugeValue, float64(stats.OpenTransactions))
	ch <- prometheus.MustNewConstMetric(c.openStatements, prometheus.GaugeValue, float64(stats.OpenStatements))
	ch <- prometheus.MustNewConstMetric(c.readBytes, prometheus.CounterValue, float64(stats.BytesRead))
	ch <- prometheus.MustNewConstMetric(c.writtenBytes, prometheus.CounterValue, float64(stats.BytesWritten))
	for i, durationStat := range stats.SQLDurations {
		ch <- prometheus.MustNewConstHistogram(c.sqlDurations, durationStat.Count, float64(durationStat.Sum), durationStatBuckets(durationStat), driver.StatsSQLTexts[i])
	}
}

// NewDriverCollector returns a collector that exports *driver.Driver metrics.
func NewDriverCollector(d *driver.Driver, dbName string) prometheus.Collector {
	return newCollector(d, "driver", prometheus.Labels{"db_name": dbName})
}

// NewConnectorCollector returns a collector that exports *driver.Connector metrics.
func NewConnectorCollector(c *driver.Connector, dbName string) prometheus.Collector {
	return newCollector(c, "connector", prometheus.Labels{"db_name": dbName})
}