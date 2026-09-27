package postgres

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// measured on postgres:18, 2026-09-27, with log_destination set to stderr,csvlog,jsonlog:
// auto_explain's entries for two statements, one over the extended protocol so its
// parameters print, the other over several lines, in each of its four formats (text,
// json, xml, yaml). The constants all say "secret", "pending", 99.50 or 7.
const (
	measuredAutoExplain = "2026-09-27 04:56:49.970 UTC [57793] LOG:  duration: 0.434 ms  plan:\n" +
		"\tQuery Text: SELECT id, amount FROM o WHERE status = $1 AND amount > 99.50 AND note <> 'plan secret' \n" +
		"\tQuery Parameters: $1 = 'pending'\n" +
		"\tBitmap Heap Scan on public.o  (cost=25.19..108.34 rows=1644 width=10)\n" +
		"\t  Output: id, amount\n" +
		"\t  Recheck Cond: ((o.status)::text = 'pending'::text)\n" +
		"\t  Filter: ((o.amount > 99.50) AND (o.note <> 'plan secret'::text))\n" +
		"\t  ->  Bitmap Index Scan on o_status  (cost=0.00..24.78 rows=1666 width=0)\n" +
		"\t        Index Cond: ((o.status)::text = 'pending'::text)\n" +
		"\tQuery Identifier: 5561098070303724986\n" +
		"2026-09-27 04:56:49.971 UTC [57793] LOG:  duration: 0.176 ms  plan:\n" +
		"\tQuery Text: SELECT count(*)\n" +
		"\t  FROM o\n" +
		"\t WHERE note = 'multi line plan secret'\n" +
		"\t   AND id > 7;\n" +
		"\tAggregate  (cost=129.00..129.01 rows=1 width=8)\n" +
		"\t  Output: count(*)\n" +
		"\t  ->  Seq Scan on public.o  (cost=0.00..129.00 rows=1 width=0)\n" +
		"\t        Output: id, status, amount, created, note, tags\n" +
		"\t        Filter: ((o.id > 7) AND (o.note = 'multi line plan secret'::text))\n" +
		"\tQuery Identifier: 1227138514444432782\n" +
		"2026-09-27 04:56:50.044 UTC [57800] LOG:  duration: 0.344 ms  plan:\n" +
		"\t{\n" +
		"\t  \"Query Text\": \"SELECT id, amount FROM o WHERE status = $1 AND amount > 99.50 AND note <> 'plan secret' \",\n" +
		"\t  \"Query Parameters\": \"$1 = 'pending'\",\n" +
		"\t  \"Plan\": {\n" +
		"\t    \"Node Type\": \"Bitmap Heap Scan\",\n" +
		"\t    \"Parallel Aware\": false,\n" +
		"\t    \"Async Capable\": false,\n" +
		"\t    \"Relation Name\": \"o\",\n" +
		"\t    \"Schema\": \"public\",\n" +
		"\t    \"Alias\": \"o\",\n" +
		"\t    \"Startup Cost\": 25.19,\n" +
		"\t    \"Total Cost\": 108.34,\n" +
		"\t    \"Plan Rows\": 1644,\n" +
		"\t    \"Plan Width\": 10,\n" +
		"\t    \"Disabled\": false,\n" +
		"\t    \"Output\": [\"id\", \"amount\"],\n" +
		"\t    \"Recheck Cond\": \"((o.status)::text = 'pending'::text)\",\n" +
		"\t    \"Filter\": \"((o.amount > 99.50) AND (o.note <> 'plan secret'::text))\",\n" +
		"\t    \"Plans\": [\n" +
		"\t      {\n" +
		"\t        \"Node Type\": \"Bitmap Index Scan\",\n" +
		"\t        \"Parent Relationship\": \"Outer\",\n" +
		"\t        \"Parallel Aware\": false,\n" +
		"\t        \"Async Capable\": false,\n" +
		"\t        \"Index Name\": \"o_status\",\n" +
		"\t        \"Startup Cost\": 0.00,\n" +
		"\t        \"Total Cost\": 24.78,\n" +
		"\t        \"Plan Rows\": 1666,\n" +
		"\t        \"Plan Width\": 0,\n" +
		"\t        \"Disabled\": false,\n" +
		"\t        \"Index Cond\": \"((o.status)::text = 'pending'::text)\"\n" +
		"\t      }\n" +
		"\t    ]\n" +
		"\t  },\n" +
		"\t  \"Query Identifier\": 5561098070303724986\n" +
		"\t}\n" +
		"2026-09-27 04:56:50.045 UTC [57800] LOG:  duration: 0.175 ms  plan:\n" +
		"\t{\n" +
		"\t  \"Query Text\": \"SELECT count(*)\\n  FROM o\\n WHERE note = 'multi line plan secret'\\n   AND id > 7;\",\n" +
		"\t  \"Plan\": {\n" +
		"\t    \"Node Type\": \"Aggregate\",\n" +
		"\t    \"Strategy\": \"Plain\",\n" +
		"\t    \"Partial Mode\": \"Simple\",\n" +
		"\t    \"Parallel Aware\": false,\n" +
		"\t    \"Async Capable\": false,\n" +
		"\t    \"Startup Cost\": 129.00,\n" +
		"\t    \"Total Cost\": 129.01,\n" +
		"\t    \"Plan Rows\": 1,\n" +
		"\t    \"Plan Width\": 8,\n" +
		"\t    \"Disabled\": false,\n" +
		"\t    \"Output\": [\"count(*)\"],\n" +
		"\t    \"Plans\": [\n" +
		"\t      {\n" +
		"\t        \"Node Type\": \"Seq Scan\",\n" +
		"\t        \"Parent Relationship\": \"Outer\",\n" +
		"\t        \"Parallel Aware\": false,\n" +
		"\t        \"Async Capable\": false,\n" +
		"\t        \"Relation Name\": \"o\",\n" +
		"\t        \"Schema\": \"public\",\n" +
		"\t        \"Alias\": \"o\",\n" +
		"\t        \"Startup Cost\": 0.00,\n" +
		"\t        \"Total Cost\": 129.00,\n" +
		"\t        \"Plan Rows\": 1,\n" +
		"\t        \"Plan Width\": 0,\n" +
		"\t        \"Disabled\": false,\n" +
		"\t        \"Output\": [\"id\", \"status\", \"amount\", \"created\", \"note\", \"tags\"],\n" +
		"\t        \"Filter\": \"((o.id > 7) AND (o.note = 'multi line plan secret'::text))\"\n" +
		"\t      }\n" +
		"\t    ]\n" +
		"\t  },\n" +
		"\t  \"Query Identifier\": 1227138514444432782\n" +
		"\t}\n" +
		"2026-09-27 04:56:50.120 UTC [57807] LOG:  duration: 0.359 ms  plan:\n" +
		"\t<explain xmlns=\"http://www.postgresql.org/2009/explain\">\n" +
		"\t  <Query-Text>SELECT id, amount FROM o WHERE status = $1 AND amount &gt; 99.50 AND note &lt;&gt; 'plan secret' </Query-Text>\n" +
		"\t  <Query-Parameters>$1 = 'pending'</Query-Parameters>\n" +
		"\t  <Plan>\n" +
		"\t    <Node-Type>Bitmap Heap Scan</Node-Type>\n" +
		"\t    <Parallel-Aware>false</Parallel-Aware>\n" +
		"\t    <Async-Capable>false</Async-Capable>\n" +
		"\t    <Relation-Name>o</Relation-Name>\n" +
		"\t    <Schema>public</Schema>\n" +
		"\t    <Alias>o</Alias>\n" +
		"\t    <Startup-Cost>25.19</Startup-Cost>\n" +
		"\t    <Total-Cost>108.34</Total-Cost>\n" +
		"\t    <Plan-Rows>1644</Plan-Rows>\n" +
		"\t    <Plan-Width>10</Plan-Width>\n" +
		"\t    <Disabled>false</Disabled>\n" +
		"\t    <Output>\n" +
		"\t      <Item>id</Item>\n" +
		"\t      <Item>amount</Item>\n" +
		"\t    </Output>\n" +
		"\t    <Recheck-Cond>((o.status)::text = 'pending'::text)</Recheck-Cond>\n" +
		"\t    <Filter>((o.amount &gt; 99.50) AND (o.note &lt;&gt; 'plan secret'::text))</Filter>\n" +
		"\t    <Plans>\n" +
		"\t      <Plan>\n" +
		"\t        <Node-Type>Bitmap Index Scan</Node-Type>\n" +
		"\t        <Parent-Relationship>Outer</Parent-Relationship>\n" +
		"\t        <Parallel-Aware>false</Parallel-Aware>\n" +
		"\t        <Async-Capable>false</Async-Capable>\n" +
		"\t        <Index-Name>o_status</Index-Name>\n" +
		"\t        <Startup-Cost>0.00</Startup-Cost>\n" +
		"\t        <Total-Cost>24.78</Total-Cost>\n" +
		"\t        <Plan-Rows>1666</Plan-Rows>\n" +
		"\t        <Plan-Width>0</Plan-Width>\n" +
		"\t        <Disabled>false</Disabled>\n" +
		"\t        <Index-Cond>((o.status)::text = 'pending'::text)</Index-Cond>\n" +
		"\t      </Plan>\n" +
		"\t    </Plans>\n" +
		"\t  </Plan>\n" +
		"\t  <Query-Identifier>5561098070303724986</Query-Identifier>\n" +
		"\t</explain>\n" +
		"2026-09-27 04:56:50.121 UTC [57807] LOG:  duration: 0.177 ms  plan:\n" +
		"\t<explain xmlns=\"http://www.postgresql.org/2009/explain\">\n" +
		"\t  <Query-Text>SELECT count(*)\n" +
		"\t  FROM o\n" +
		"\t WHERE note = 'multi line plan secret'\n" +
		"\t   AND id &gt; 7;</Query-Text>\n" +
		"\t  <Plan>\n" +
		"\t    <Node-Type>Aggregate</Node-Type>\n" +
		"\t    <Strategy>Plain</Strategy>\n" +
		"\t    <Partial-Mode>Simple</Partial-Mode>\n" +
		"\t    <Parallel-Aware>false</Parallel-Aware>\n" +
		"\t    <Async-Capable>false</Async-Capable>\n" +
		"\t    <Startup-Cost>129.00</Startup-Cost>\n" +
		"\t    <Total-Cost>129.01</Total-Cost>\n" +
		"\t    <Plan-Rows>1</Plan-Rows>\n" +
		"\t    <Plan-Width>8</Plan-Width>\n" +
		"\t    <Disabled>false</Disabled>\n" +
		"\t    <Output>\n" +
		"\t      <Item>count(*)</Item>\n" +
		"\t    </Output>\n" +
		"\t    <Plans>\n" +
		"\t      <Plan>\n" +
		"\t        <Node-Type>Seq Scan</Node-Type>\n" +
		"\t        <Parent-Relationship>Outer</Parent-Relationship>\n" +
		"\t        <Parallel-Aware>false</Parallel-Aware>\n" +
		"\t        <Async-Capable>false</Async-Capable>\n" +
		"\t        <Relation-Name>o</Relation-Name>\n" +
		"\t        <Schema>public</Schema>\n" +
		"\t        <Alias>o</Alias>\n" +
		"\t        <Startup-Cost>0.00</Startup-Cost>\n" +
		"\t        <Total-Cost>129.00</Total-Cost>\n" +
		"\t        <Plan-Rows>1</Plan-Rows>\n" +
		"\t        <Plan-Width>0</Plan-Width>\n" +
		"\t        <Disabled>false</Disabled>\n" +
		"\t        <Output>\n" +
		"\t          <Item>id</Item>\n" +
		"\t          <Item>status</Item>\n" +
		"\t          <Item>amount</Item>\n" +
		"\t          <Item>created</Item>\n" +
		"\t          <Item>note</Item>\n" +
		"\t          <Item>tags</Item>\n" +
		"\t        </Output>\n" +
		"\t        <Filter>((o.id &gt; 7) AND (o.note = 'multi line plan secret'::text))</Filter>\n" +
		"\t      </Plan>\n" +
		"\t    </Plans>\n" +
		"\t  </Plan>\n" +
		"\t  <Query-Identifier>1227138514444432782</Query-Identifier>\n" +
		"\t</explain>\n" +
		"2026-09-27 04:56:50.193 UTC [57814] LOG:  duration: 0.351 ms  plan:\n" +
		"\tQuery Text: \"SELECT id, amount FROM o WHERE status = $1 AND amount > 99.50 AND note <> 'plan secret' \"\n" +
		"\tQuery Parameters: \"$1 = 'pending'\"\n" +
		"\tPlan: \n" +
		"\t  Node Type: \"Bitmap Heap Scan\"\n" +
		"\t  Parallel Aware: false\n" +
		"\t  Async Capable: false\n" +
		"\t  Relation Name: \"o\"\n" +
		"\t  Schema: \"public\"\n" +
		"\t  Alias: \"o\"\n" +
		"\t  Startup Cost: 25.19\n" +
		"\t  Total Cost: 108.34\n" +
		"\t  Plan Rows: 1644\n" +
		"\t  Plan Width: 10\n" +
		"\t  Disabled: false\n" +
		"\t  Output: \n" +
		"\t    - \"id\"\n" +
		"\t    - \"amount\"\n" +
		"\t  Recheck Cond: \"((o.status)::text = 'pending'::text)\"\n" +
		"\t  Filter: \"((o.amount > 99.50) AND (o.note <> 'plan secret'::text))\"\n" +
		"\t  Plans: \n" +
		"\t    - Node Type: \"Bitmap Index Scan\"\n" +
		"\t      Parent Relationship: \"Outer\"\n" +
		"\t      Parallel Aware: false\n" +
		"\t      Async Capable: false\n" +
		"\t      Index Name: \"o_status\"\n" +
		"\t      Startup Cost: 0.00\n" +
		"\t      Total Cost: 24.78\n" +
		"\t      Plan Rows: 1666\n" +
		"\t      Plan Width: 0\n" +
		"\t      Disabled: false\n" +
		"\t      Index Cond: \"((o.status)::text = 'pending'::text)\"\n" +
		"\tQuery Identifier: 5561098070303724986\n" +
		"2026-09-27 04:56:50.194 UTC [57814] LOG:  duration: 0.195 ms  plan:\n" +
		"\tQuery Text: \"SELECT count(*)\\n  FROM o\\n WHERE note = 'multi line plan secret'\\n   AND id > 7;\"\n" +
		"\tPlan: \n" +
		"\t  Node Type: \"Aggregate\"\n" +
		"\t  Strategy: \"Plain\"\n" +
		"\t  Partial Mode: \"Simple\"\n" +
		"\t  Parallel Aware: false\n" +
		"\t  Async Capable: false\n" +
		"\t  Startup Cost: 129.00\n" +
		"\t  Total Cost: 129.01\n" +
		"\t  Plan Rows: 1\n" +
		"\t  Plan Width: 8\n" +
		"\t  Disabled: false\n" +
		"\t  Output: \n" +
		"\t    - \"count(*)\"\n" +
		"\t  Plans: \n" +
		"\t    - Node Type: \"Seq Scan\"\n" +
		"\t      Parent Relationship: \"Outer\"\n" +
		"\t      Parallel Aware: false\n" +
		"\t      Async Capable: false\n" +
		"\t      Relation Name: \"o\"\n" +
		"\t      Schema: \"public\"\n" +
		"\t      Alias: \"o\"\n" +
		"\t      Startup Cost: 0.00\n" +
		"\t      Total Cost: 129.00\n" +
		"\t      Plan Rows: 1\n" +
		"\t      Plan Width: 0\n" +
		"\t      Disabled: false\n" +
		"\t      Output: \n" +
		"\t        - \"id\"\n" +
		"\t        - \"status\"\n" +
		"\t        - \"amount\"\n" +
		"\t        - \"created\"\n" +
		"\t        - \"note\"\n" +
		"\t        - \"tags\"\n" +
		"\t      Filter: \"((o.id > 7) AND (o.note = 'multi line plan secret'::text))\"\n" +
		"\tQuery Identifier: 1227138514444432782\n"

	measuredAutoExplainCSV = `2026-09-27 04:56:49.970 UTC,"postgres","yc_plan_measure",57793,"[local]",6ab8a211.e1c1,1,"SELECT",2026-09-27 04:56:49 UTC,92/540,0,LOG,00000,"duration: 0.434 ms  plan:
Query Text: SELECT id, amount FROM o WHERE status = $1 AND amount > 99.50 AND note <> 'plan secret' 
Query Parameters: $1 = 'pending'
Bitmap Heap Scan on public.o  (cost=25.19..108.34 rows=1644 width=10)
  Output: id, amount
  Recheck Cond: ((o.status)::text = 'pending'::text)
  Filter: ((o.amount > 99.50) AND (o.note <> 'plan secret'::text))
  ->  Bitmap Index Scan on o_status  (cost=0.00..24.78 rows=1666 width=0)
        Index Cond: ((o.status)::text = 'pending'::text)
Query Identifier: 5561098070303724986",,,,,,,,,"psql","client backend",,5561098070303724986` + "\n" +
		`2026-09-27 04:56:49.971 UTC,"postgres","yc_plan_measure",57793,"[local]",6ab8a211.e1c1,2,"SELECT",2026-09-27 04:56:49 UTC,92/541,0,LOG,00000,"duration: 0.176 ms  plan:
Query Text: SELECT count(*)
  FROM o
 WHERE note = 'multi line plan secret'
   AND id > 7;
Aggregate  (cost=129.00..129.01 rows=1 width=8)
  Output: count(*)
  ->  Seq Scan on public.o  (cost=0.00..129.00 rows=1 width=0)
        Output: id, status, amount, created, note, tags
        Filter: ((o.id > 7) AND (o.note = 'multi line plan secret'::text))
Query Identifier: 1227138514444432782",,,,,,,,,"psql","client backend",,1227138514444432782` + "\n" +
		`2026-09-27 04:56:50.044 UTC,"postgres","yc_plan_measure",57800,"[local]",6ab8a212.e1c8,1,"SELECT",2026-09-27 04:56:50 UTC,69/763,0,LOG,00000,"duration: 0.344 ms  plan:
{
  ""Query Text"": ""SELECT id, amount FROM o WHERE status = $1 AND amount > 99.50 AND note <> 'plan secret' "",
  ""Query Parameters"": ""$1 = 'pending'"",
  ""Plan"": {
    ""Node Type"": ""Bitmap Heap Scan"",
    ""Parallel Aware"": false,
    ""Async Capable"": false,
    ""Relation Name"": ""o"",
    ""Schema"": ""public"",
    ""Alias"": ""o"",
    ""Startup Cost"": 25.19,
    ""Total Cost"": 108.34,
    ""Plan Rows"": 1644,
    ""Plan Width"": 10,
    ""Disabled"": false,
    ""Output"": [""id"", ""amount""],
    ""Recheck Cond"": ""((o.status)::text = 'pending'::text)"",
    ""Filter"": ""((o.amount > 99.50) AND (o.note <> 'plan secret'::text))"",
    ""Plans"": [
      {
        ""Node Type"": ""Bitmap Index Scan"",
        ""Parent Relationship"": ""Outer"",
        ""Parallel Aware"": false,
        ""Async Capable"": false,
        ""Index Name"": ""o_status"",
        ""Startup Cost"": 0.00,
        ""Total Cost"": 24.78,
        ""Plan Rows"": 1666,
        ""Plan Width"": 0,
        ""Disabled"": false,
        ""Index Cond"": ""((o.status)::text = 'pending'::text)""
      }
    ]
  },
  ""Query Identifier"": 5561098070303724986
}",,,,,,,,,"psql","client backend",,5561098070303724986` + "\n" +
		`2026-09-27 04:56:50.045 UTC,"postgres","yc_plan_measure",57800,"[local]",6ab8a212.e1c8,2,"SELECT",2026-09-27 04:56:50 UTC,69/764,0,LOG,00000,"duration: 0.175 ms  plan:
{
  ""Query Text"": ""SELECT count(*)\n  FROM o\n WHERE note = 'multi line plan secret'\n   AND id > 7;"",
  ""Plan"": {
    ""Node Type"": ""Aggregate"",
    ""Strategy"": ""Plain"",
    ""Partial Mode"": ""Simple"",
    ""Parallel Aware"": false,
    ""Async Capable"": false,
    ""Startup Cost"": 129.00,
    ""Total Cost"": 129.01,
    ""Plan Rows"": 1,
    ""Plan Width"": 8,
    ""Disabled"": false,
    ""Output"": [""count(*)""],
    ""Plans"": [
      {
        ""Node Type"": ""Seq Scan"",
        ""Parent Relationship"": ""Outer"",
        ""Parallel Aware"": false,
        ""Async Capable"": false,
        ""Relation Name"": ""o"",
        ""Schema"": ""public"",
        ""Alias"": ""o"",
        ""Startup Cost"": 0.00,
        ""Total Cost"": 129.00,
        ""Plan Rows"": 1,
        ""Plan Width"": 0,
        ""Disabled"": false,
        ""Output"": [""id"", ""status"", ""amount"", ""created"", ""note"", ""tags""],
        ""Filter"": ""((o.id > 7) AND (o.note = 'multi line plan secret'::text))""
      }
    ]
  },
  ""Query Identifier"": 1227138514444432782
}",,,,,,,,,"psql","client backend",,1227138514444432782` + "\n" +
		`2026-09-27 04:56:50.120 UTC,"postgres","yc_plan_measure",57807,"[local]",6ab8a212.e1cf,1,"SELECT",2026-09-27 04:56:50 UTC,26/621,0,LOG,00000,"duration: 0.359 ms  plan:
<explain xmlns=""http://www.postgresql.org/2009/explain"">
  <Query-Text>SELECT id, amount FROM o WHERE status = $1 AND amount &gt; 99.50 AND note &lt;&gt; 'plan secret' </Query-Text>
  <Query-Parameters>$1 = 'pending'</Query-Parameters>
  <Plan>
    <Node-Type>Bitmap Heap Scan</Node-Type>
    <Parallel-Aware>false</Parallel-Aware>
    <Async-Capable>false</Async-Capable>
    <Relation-Name>o</Relation-Name>
    <Schema>public</Schema>
    <Alias>o</Alias>
    <Startup-Cost>25.19</Startup-Cost>
    <Total-Cost>108.34</Total-Cost>
    <Plan-Rows>1644</Plan-Rows>
    <Plan-Width>10</Plan-Width>
    <Disabled>false</Disabled>
    <Output>
      <Item>id</Item>
      <Item>amount</Item>
    </Output>
    <Recheck-Cond>((o.status)::text = 'pending'::text)</Recheck-Cond>
    <Filter>((o.amount &gt; 99.50) AND (o.note &lt;&gt; 'plan secret'::text))</Filter>
    <Plans>
      <Plan>
        <Node-Type>Bitmap Index Scan</Node-Type>
        <Parent-Relationship>Outer</Parent-Relationship>
        <Parallel-Aware>false</Parallel-Aware>
        <Async-Capable>false</Async-Capable>
        <Index-Name>o_status</Index-Name>
        <Startup-Cost>0.00</Startup-Cost>
        <Total-Cost>24.78</Total-Cost>
        <Plan-Rows>1666</Plan-Rows>
        <Plan-Width>0</Plan-Width>
        <Disabled>false</Disabled>
        <Index-Cond>((o.status)::text = 'pending'::text)</Index-Cond>
      </Plan>
    </Plans>
  </Plan>
  <Query-Identifier>5561098070303724986</Query-Identifier>
</explain>",,,,,,,,,"psql","client backend",,5561098070303724986` + "\n" +
		`2026-09-27 04:56:50.121 UTC,"postgres","yc_plan_measure",57807,"[local]",6ab8a212.e1cf,2,"SELECT",2026-09-27 04:56:50 UTC,26/622,0,LOG,00000,"duration: 0.177 ms  plan:
<explain xmlns=""http://www.postgresql.org/2009/explain"">
  <Query-Text>SELECT count(*)
  FROM o
 WHERE note = 'multi line plan secret'
   AND id &gt; 7;</Query-Text>
  <Plan>
    <Node-Type>Aggregate</Node-Type>
    <Strategy>Plain</Strategy>
    <Partial-Mode>Simple</Partial-Mode>
    <Parallel-Aware>false</Parallel-Aware>
    <Async-Capable>false</Async-Capable>
    <Startup-Cost>129.00</Startup-Cost>
    <Total-Cost>129.01</Total-Cost>
    <Plan-Rows>1</Plan-Rows>
    <Plan-Width>8</Plan-Width>
    <Disabled>false</Disabled>
    <Output>
      <Item>count(*)</Item>
    </Output>
    <Plans>
      <Plan>
        <Node-Type>Seq Scan</Node-Type>
        <Parent-Relationship>Outer</Parent-Relationship>
        <Parallel-Aware>false</Parallel-Aware>
        <Async-Capable>false</Async-Capable>
        <Relation-Name>o</Relation-Name>
        <Schema>public</Schema>
        <Alias>o</Alias>
        <Startup-Cost>0.00</Startup-Cost>
        <Total-Cost>129.00</Total-Cost>
        <Plan-Rows>1</Plan-Rows>
        <Plan-Width>0</Plan-Width>
        <Disabled>false</Disabled>
        <Output>
          <Item>id</Item>
          <Item>status</Item>
          <Item>amount</Item>
          <Item>created</Item>
          <Item>note</Item>
          <Item>tags</Item>
        </Output>
        <Filter>((o.id &gt; 7) AND (o.note = 'multi line plan secret'::text))</Filter>
      </Plan>
    </Plans>
  </Plan>
  <Query-Identifier>1227138514444432782</Query-Identifier>
</explain>",,,,,,,,,"psql","client backend",,1227138514444432782` + "\n" +
		`2026-09-27 04:56:50.193 UTC,"postgres","yc_plan_measure",57814,"[local]",6ab8a212.e1d6,1,"SELECT",2026-09-27 04:56:50 UTC,30/543,0,LOG,00000,"duration: 0.351 ms  plan:
Query Text: ""SELECT id, amount FROM o WHERE status = $1 AND amount > 99.50 AND note <> 'plan secret' ""
Query Parameters: ""$1 = 'pending'""
Plan: 
  Node Type: ""Bitmap Heap Scan""
  Parallel Aware: false
  Async Capable: false
  Relation Name: ""o""
  Schema: ""public""
  Alias: ""o""
  Startup Cost: 25.19
  Total Cost: 108.34
  Plan Rows: 1644
  Plan Width: 10
  Disabled: false
  Output: 
    - ""id""
    - ""amount""
  Recheck Cond: ""((o.status)::text = 'pending'::text)""
  Filter: ""((o.amount > 99.50) AND (o.note <> 'plan secret'::text))""
  Plans: 
    - Node Type: ""Bitmap Index Scan""
      Parent Relationship: ""Outer""
      Parallel Aware: false
      Async Capable: false
      Index Name: ""o_status""
      Startup Cost: 0.00
      Total Cost: 24.78
      Plan Rows: 1666
      Plan Width: 0
      Disabled: false
      Index Cond: ""((o.status)::text = 'pending'::text)""
Query Identifier: 5561098070303724986",,,,,,,,,"psql","client backend",,5561098070303724986` + "\n" +
		`2026-09-27 04:56:50.194 UTC,"postgres","yc_plan_measure",57814,"[local]",6ab8a212.e1d6,2,"SELECT",2026-09-27 04:56:50 UTC,30/544,0,LOG,00000,"duration: 0.195 ms  plan:
Query Text: ""SELECT count(*)\n  FROM o\n WHERE note = 'multi line plan secret'\n   AND id > 7;""
Plan: 
  Node Type: ""Aggregate""
  Strategy: ""Plain""
  Partial Mode: ""Simple""
  Parallel Aware: false
  Async Capable: false
  Startup Cost: 129.00
  Total Cost: 129.01
  Plan Rows: 1
  Plan Width: 8
  Disabled: false
  Output: 
    - ""count(*)""
  Plans: 
    - Node Type: ""Seq Scan""
      Parent Relationship: ""Outer""
      Parallel Aware: false
      Async Capable: false
      Relation Name: ""o""
      Schema: ""public""
      Alias: ""o""
      Startup Cost: 0.00
      Total Cost: 129.00
      Plan Rows: 1
      Plan Width: 0
      Disabled: false
      Output: 
        - ""id""
        - ""status""
        - ""amount""
        - ""created""
        - ""note""
        - ""tags""
      Filter: ""((o.id > 7) AND (o.note = 'multi line plan secret'::text))""
Query Identifier: 1227138514444432782",,,,,,,,,"psql","client backend",,1227138514444432782` + "\n"

	measuredAutoExplainJSON = `{"timestamp":"2026-09-27 04:56:49.970 UTC","user":"postgres","dbname":"yc_plan_measure","pid":57793,"remote_host":"[local]","session_id":"6ab8a211.e1c1","line_num":1,"ps":"SELECT","session_start":"2026-09-27 04:56:49 UTC","vxid":"92/540","txid":0,"error_severity":"LOG","message":"duration: 0.434 ms  plan:\nQuery Text: SELECT id, amount FROM o WHERE status = $1 AND amount > 99.50 AND note <> 'plan secret' \nQuery Parameters: $1 = 'pending'\nBitmap Heap Scan on public.o  (cost=25.19..108.34 rows=1644 width=10)\n  Output: id, amount\n  Recheck Cond: ((o.status)::text = 'pending'::text)\n  Filter: ((o.amount > 99.50) AND (o.note <> 'plan secret'::text))\n  ->  Bitmap Index Scan on o_status  (cost=0.00..24.78 rows=1666 width=0)\n        Index Cond: ((o.status)::text = 'pending'::text)\nQuery Identifier: 5561098070303724986","application_name":"psql","backend_type":"client backend","query_id":5561098070303724986}` + "\n" +
		`{"timestamp":"2026-09-27 04:56:49.971 UTC","user":"postgres","dbname":"yc_plan_measure","pid":57793,"remote_host":"[local]","session_id":"6ab8a211.e1c1","line_num":2,"ps":"SELECT","session_start":"2026-09-27 04:56:49 UTC","vxid":"92/541","txid":0,"error_severity":"LOG","message":"duration: 0.176 ms  plan:\nQuery Text: SELECT count(*)\n  FROM o\n WHERE note = 'multi line plan secret'\n   AND id > 7;\nAggregate  (cost=129.00..129.01 rows=1 width=8)\n  Output: count(*)\n  ->  Seq Scan on public.o  (cost=0.00..129.00 rows=1 width=0)\n        Output: id, status, amount, created, note, tags\n        Filter: ((o.id > 7) AND (o.note = 'multi line plan secret'::text))\nQuery Identifier: 1227138514444432782","application_name":"psql","backend_type":"client backend","query_id":1227138514444432782}` + "\n" +
		`{"timestamp":"2026-09-27 04:56:50.044 UTC","user":"postgres","dbname":"yc_plan_measure","pid":57800,"remote_host":"[local]","session_id":"6ab8a212.e1c8","line_num":1,"ps":"SELECT","session_start":"2026-09-27 04:56:50 UTC","vxid":"69/763","txid":0,"error_severity":"LOG","message":"duration: 0.344 ms  plan:\n{\n  \"Query Text\": \"SELECT id, amount FROM o WHERE status = $1 AND amount > 99.50 AND note <> 'plan secret' \",\n  \"Query Parameters\": \"$1 = 'pending'\",\n  \"Plan\": {\n    \"Node Type\": \"Bitmap Heap Scan\",\n    \"Parallel Aware\": false,\n    \"Async Capable\": false,\n    \"Relation Name\": \"o\",\n    \"Schema\": \"public\",\n    \"Alias\": \"o\",\n    \"Startup Cost\": 25.19,\n    \"Total Cost\": 108.34,\n    \"Plan Rows\": 1644,\n    \"Plan Width\": 10,\n    \"Disabled\": false,\n    \"Output\": [\"id\", \"amount\"],\n    \"Recheck Cond\": \"((o.status)::text = 'pending'::text)\",\n    \"Filter\": \"((o.amount > 99.50) AND (o.note <> 'plan secret'::text))\",\n    \"Plans\": [\n      {\n        \"Node Type\": \"Bitmap Index Scan\",\n        \"Parent Relationship\": \"Outer\",\n        \"Parallel Aware\": false,\n        \"Async Capable\": false,\n        \"Index Name\": \"o_status\",\n        \"Startup Cost\": 0.00,\n        \"Total Cost\": 24.78,\n        \"Plan Rows\": 1666,\n        \"Plan Width\": 0,\n        \"Disabled\": false,\n        \"Index Cond\": \"((o.status)::text = 'pending'::text)\"\n      }\n    ]\n  },\n  \"Query Identifier\": 5561098070303724986\n}","application_name":"psql","backend_type":"client backend","query_id":5561098070303724986}` + "\n" +
		`{"timestamp":"2026-09-27 04:56:50.045 UTC","user":"postgres","dbname":"yc_plan_measure","pid":57800,"remote_host":"[local]","session_id":"6ab8a212.e1c8","line_num":2,"ps":"SELECT","session_start":"2026-09-27 04:56:50 UTC","vxid":"69/764","txid":0,"error_severity":"LOG","message":"duration: 0.175 ms  plan:\n{\n  \"Query Text\": \"SELECT count(*)\\n  FROM o\\n WHERE note = 'multi line plan secret'\\n   AND id > 7;\",\n  \"Plan\": {\n    \"Node Type\": \"Aggregate\",\n    \"Strategy\": \"Plain\",\n    \"Partial Mode\": \"Simple\",\n    \"Parallel Aware\": false,\n    \"Async Capable\": false,\n    \"Startup Cost\": 129.00,\n    \"Total Cost\": 129.01,\n    \"Plan Rows\": 1,\n    \"Plan Width\": 8,\n    \"Disabled\": false,\n    \"Output\": [\"count(*)\"],\n    \"Plans\": [\n      {\n        \"Node Type\": \"Seq Scan\",\n        \"Parent Relationship\": \"Outer\",\n        \"Parallel Aware\": false,\n        \"Async Capable\": false,\n        \"Relation Name\": \"o\",\n        \"Schema\": \"public\",\n        \"Alias\": \"o\",\n        \"Startup Cost\": 0.00,\n        \"Total Cost\": 129.00,\n        \"Plan Rows\": 1,\n        \"Plan Width\": 0,\n        \"Disabled\": false,\n        \"Output\": [\"id\", \"status\", \"amount\", \"created\", \"note\", \"tags\"],\n        \"Filter\": \"((o.id > 7) AND (o.note = 'multi line plan secret'::text))\"\n      }\n    ]\n  },\n  \"Query Identifier\": 1227138514444432782\n}","application_name":"psql","backend_type":"client backend","query_id":1227138514444432782}` + "\n" +
		`{"timestamp":"2026-09-27 04:56:50.120 UTC","user":"postgres","dbname":"yc_plan_measure","pid":57807,"remote_host":"[local]","session_id":"6ab8a212.e1cf","line_num":1,"ps":"SELECT","session_start":"2026-09-27 04:56:50 UTC","vxid":"26/621","txid":0,"error_severity":"LOG","message":"duration: 0.359 ms  plan:\n<explain xmlns=\"http://www.postgresql.org/2009/explain\">\n  <Query-Text>SELECT id, amount FROM o WHERE status = $1 AND amount &gt; 99.50 AND note &lt;&gt; 'plan secret' </Query-Text>\n  <Query-Parameters>$1 = 'pending'</Query-Parameters>\n  <Plan>\n    <Node-Type>Bitmap Heap Scan</Node-Type>\n    <Parallel-Aware>false</Parallel-Aware>\n    <Async-Capable>false</Async-Capable>\n    <Relation-Name>o</Relation-Name>\n    <Schema>public</Schema>\n    <Alias>o</Alias>\n    <Startup-Cost>25.19</Startup-Cost>\n    <Total-Cost>108.34</Total-Cost>\n    <Plan-Rows>1644</Plan-Rows>\n    <Plan-Width>10</Plan-Width>\n    <Disabled>false</Disabled>\n    <Output>\n      <Item>id</Item>\n      <Item>amount</Item>\n    </Output>\n    <Recheck-Cond>((o.status)::text = 'pending'::text)</Recheck-Cond>\n    <Filter>((o.amount &gt; 99.50) AND (o.note &lt;&gt; 'plan secret'::text))</Filter>\n    <Plans>\n      <Plan>\n        <Node-Type>Bitmap Index Scan</Node-Type>\n        <Parent-Relationship>Outer</Parent-Relationship>\n        <Parallel-Aware>false</Parallel-Aware>\n        <Async-Capable>false</Async-Capable>\n        <Index-Name>o_status</Index-Name>\n        <Startup-Cost>0.00</Startup-Cost>\n        <Total-Cost>24.78</Total-Cost>\n        <Plan-Rows>1666</Plan-Rows>\n        <Plan-Width>0</Plan-Width>\n        <Disabled>false</Disabled>\n        <Index-Cond>((o.status)::text = 'pending'::text)</Index-Cond>\n      </Plan>\n    </Plans>\n  </Plan>\n  <Query-Identifier>5561098070303724986</Query-Identifier>\n</explain>","application_name":"psql","backend_type":"client backend","query_id":5561098070303724986}` + "\n" +
		`{"timestamp":"2026-09-27 04:56:50.121 UTC","user":"postgres","dbname":"yc_plan_measure","pid":57807,"remote_host":"[local]","session_id":"6ab8a212.e1cf","line_num":2,"ps":"SELECT","session_start":"2026-09-27 04:56:50 UTC","vxid":"26/622","txid":0,"error_severity":"LOG","message":"duration: 0.177 ms  plan:\n<explain xmlns=\"http://www.postgresql.org/2009/explain\">\n  <Query-Text>SELECT count(*)\n  FROM o\n WHERE note = 'multi line plan secret'\n   AND id &gt; 7;</Query-Text>\n  <Plan>\n    <Node-Type>Aggregate</Node-Type>\n    <Strategy>Plain</Strategy>\n    <Partial-Mode>Simple</Partial-Mode>\n    <Parallel-Aware>false</Parallel-Aware>\n    <Async-Capable>false</Async-Capable>\n    <Startup-Cost>129.00</Startup-Cost>\n    <Total-Cost>129.01</Total-Cost>\n    <Plan-Rows>1</Plan-Rows>\n    <Plan-Width>8</Plan-Width>\n    <Disabled>false</Disabled>\n    <Output>\n      <Item>count(*)</Item>\n    </Output>\n    <Plans>\n      <Plan>\n        <Node-Type>Seq Scan</Node-Type>\n        <Parent-Relationship>Outer</Parent-Relationship>\n        <Parallel-Aware>false</Parallel-Aware>\n        <Async-Capable>false</Async-Capable>\n        <Relation-Name>o</Relation-Name>\n        <Schema>public</Schema>\n        <Alias>o</Alias>\n        <Startup-Cost>0.00</Startup-Cost>\n        <Total-Cost>129.00</Total-Cost>\n        <Plan-Rows>1</Plan-Rows>\n        <Plan-Width>0</Plan-Width>\n        <Disabled>false</Disabled>\n        <Output>\n          <Item>id</Item>\n          <Item>status</Item>\n          <Item>amount</Item>\n          <Item>created</Item>\n          <Item>note</Item>\n          <Item>tags</Item>\n        </Output>\n        <Filter>((o.id &gt; 7) AND (o.note = 'multi line plan secret'::text))</Filter>\n      </Plan>\n    </Plans>\n  </Plan>\n  <Query-Identifier>1227138514444432782</Query-Identifier>\n</explain>","application_name":"psql","backend_type":"client backend","query_id":1227138514444432782}` + "\n" +
		`{"timestamp":"2026-09-27 04:56:50.193 UTC","user":"postgres","dbname":"yc_plan_measure","pid":57814,"remote_host":"[local]","session_id":"6ab8a212.e1d6","line_num":1,"ps":"SELECT","session_start":"2026-09-27 04:56:50 UTC","vxid":"30/543","txid":0,"error_severity":"LOG","message":"duration: 0.351 ms  plan:\nQuery Text: \"SELECT id, amount FROM o WHERE status = $1 AND amount > 99.50 AND note <> 'plan secret' \"\nQuery Parameters: \"$1 = 'pending'\"\nPlan: \n  Node Type: \"Bitmap Heap Scan\"\n  Parallel Aware: false\n  Async Capable: false\n  Relation Name: \"o\"\n  Schema: \"public\"\n  Alias: \"o\"\n  Startup Cost: 25.19\n  Total Cost: 108.34\n  Plan Rows: 1644\n  Plan Width: 10\n  Disabled: false\n  Output: \n    - \"id\"\n    - \"amount\"\n  Recheck Cond: \"((o.status)::text = 'pending'::text)\"\n  Filter: \"((o.amount > 99.50) AND (o.note <> 'plan secret'::text))\"\n  Plans: \n    - Node Type: \"Bitmap Index Scan\"\n      Parent Relationship: \"Outer\"\n      Parallel Aware: false\n      Async Capable: false\n      Index Name: \"o_status\"\n      Startup Cost: 0.00\n      Total Cost: 24.78\n      Plan Rows: 1666\n      Plan Width: 0\n      Disabled: false\n      Index Cond: \"((o.status)::text = 'pending'::text)\"\nQuery Identifier: 5561098070303724986","application_name":"psql","backend_type":"client backend","query_id":5561098070303724986}` + "\n" +
		`{"timestamp":"2026-09-27 04:56:50.194 UTC","user":"postgres","dbname":"yc_plan_measure","pid":57814,"remote_host":"[local]","session_id":"6ab8a212.e1d6","line_num":2,"ps":"SELECT","session_start":"2026-09-27 04:56:50 UTC","vxid":"30/544","txid":0,"error_severity":"LOG","message":"duration: 0.195 ms  plan:\nQuery Text: \"SELECT count(*)\\n  FROM o\\n WHERE note = 'multi line plan secret'\\n   AND id > 7;\"\nPlan: \n  Node Type: \"Aggregate\"\n  Strategy: \"Plain\"\n  Partial Mode: \"Simple\"\n  Parallel Aware: false\n  Async Capable: false\n  Startup Cost: 129.00\n  Total Cost: 129.01\n  Plan Rows: 1\n  Plan Width: 8\n  Disabled: false\n  Output: \n    - \"count(*)\"\n  Plans: \n    - Node Type: \"Seq Scan\"\n      Parent Relationship: \"Outer\"\n      Parallel Aware: false\n      Async Capable: false\n      Relation Name: \"o\"\n      Schema: \"public\"\n      Alias: \"o\"\n      Startup Cost: 0.00\n      Total Cost: 129.00\n      Plan Rows: 1\n      Plan Width: 0\n      Disabled: false\n      Output: \n        - \"id\"\n        - \"status\"\n        - \"amount\"\n        - \"created\"\n        - \"note\"\n        - \"tags\"\n      Filter: \"((o.id > 7) AND (o.note = 'multi line plan secret'::text))\"\nQuery Identifier: 1227138514444432782","application_name":"psql","backend_type":"client backend","query_id":1227138514444432782}` + "\n"
)

// measuredVerbosePlans is EXPLAIN (VERBOSE, SETTINGS), the agent's own EXPLAIN, for
// statements with a constant in each kind of expression, measured on postgres:18,
// 2026-09-27; the last two are one prepared statement's generic and custom plans.
const measuredVerbosePlans = "Bitmap Heap Scan on public.o  (cost=24.86..117.90 rows=311 width=58)\n" +
	"  Output: id, (status)::character varying(10), (amount)::numeric(12,3)\n" +
	"  Recheck Cond: ((o.status)::text = 'pending'::text)\n" +
	"  Filter: ((o.amount > 99.50) AND (o.created > '2026-02-01 00:00:00'::timestamp without time zone) AND (o.note ~~ 'n1%'::text) AND (o.id <> '-5'::integer))\n" +
	"  ->  Bitmap Index Scan on o_status  (cost=0.00..24.78 rows=1666 width=0)\n" +
	"        Index Cond: ((o.status)::text = 'pending'::text)\n" +
	"Query Identifier: -9097347598987225871\n" +
	"Bitmap Heap Scan on public.o  (cost=34.36..113.69 rows=1681 width=53)\n" +
	"  Output: id, status, amount, created, note, tags\n" +
	"  Recheck Cond: (((o.status)::text = ANY ('{pending,void}'::text[])) OR (o.created < '2026-01-02 00:00:00'::timestamp without time zone))\n" +
	"  ->  BitmapOr  (cost=34.36..34.36 rows=1689 width=0)\n" +
	"        ->  Bitmap Index Scan on o_status  (cost=0.00..29.06 rows=1666 width=0)\n" +
	"              Index Cond: ((o.status)::text = ANY ('{pending,void}'::text[]))\n" +
	"        ->  Bitmap Index Scan on o_created  (cost=0.00..4.46 rows=23 width=0)\n" +
	"              Index Cond: (o.created < '2026-01-02 00:00:00'::timestamp without time zone)\n" +
	"Query Identifier: -6893059632216811227\n" +
	"Sort  (cost=658.61..658.61 rows=1 width=13)\n" +
	"  Output: o.status, (count(*))\n" +
	"  Sort Key: o.status DESC\n" +
	"  ->  HashAggregate  (cost=658.57..658.60 rows=1 width=13)\n" +
	"        Output: o.status, count(*)\n" +
	"        Group Key: o.status\n" +
	"        Filter: (count(*) > 10)\n" +
	"        ->  Hash Join  (cost=166.50..616.57 rows=8400 width=5)\n" +
	"              Output: o.status\n" +
	"              Inner Unique: true\n" +
	"              Hash Cond: (li.o_id = o.id)\n" +
	"              ->  Seq Scan on public.li  (cost=0.00..428.00 rows=8400 width=4)\n" +
	"                    Output: li.id, li.o_id, li.sku, li.qty\n" +
	"                    Filter: ((li.qty > 3) AND (li.sku <> 'sku-7'::text))\n" +
	"              ->  Hash  (cost=104.00..104.00 rows=5000 width=9)\n" +
	"                    Output: o.status, o.id\n" +
	"                    ->  Seq Scan on public.o  (cost=0.00..104.00 rows=5000 width=9)\n" +
	"                          Output: o.status, o.id\n" +
	"Query Identifier: -4551213668281853340\n" +
	"Merge Join  (cost=116.80..1083.05 rows=4 width=71)\n" +
	"  Output: o.id, o.status, o.amount, o.created, o.note, o.tags, li.id, li.o_id, li.sku, li.qty\n" +
	"  Inner Unique: true\n" +
	"  Merge Cond: (li.o_id = o.id)\n" +
	"  ->  Index Scan using li_o on public.li  (cost=0.29..916.50 rows=20000 width=18)\n" +
	"        Output: li.id, li.o_id, li.sku, li.qty\n" +
	"  ->  Sort  (cost=116.51..116.52 rows=1 width=53)\n" +
	"        Output: o.id, o.status, o.amount, o.created, o.note, o.tags\n" +
	"        Sort Key: o.id\n" +
	"        ->  Seq Scan on public.o  (cost=0.00..116.50 rows=1 width=53)\n" +
	"              Output: o.id, o.status, o.amount, o.created, o.note, o.tags\n" +
	"              Filter: (o.amount = 3.00)\n" +
	"Settings: enable_hashjoin = 'off', enable_nestloop = 'off'\n" +
	"Query Identifier: 5061214248913993263\n" +
	"Tid Scan on public.o  (cost=0.00..4.01 rows=1 width=53)\n" +
	"  Output: id, status, amount, created, note, tags\n" +
	"  TID Cond: (o.ctid = '(0,1)'::tid)\n" +
	"Query Identifier: 8476259892483075551\n" +
	"Result  (cost=0.00..0.01 rows=1 width=4)\n" +
	"  Output: 1\n" +
	"  One-Time Filter: false\n" +
	"Query Identifier: -1570266832588058360\n" +
	"Function Scan on pg_catalog.generate_series g  (cost=0.00..0.05 rows=5 width=4)\n" +
	"  Output: g\n" +
	"  Function Call: generate_series(1, 10, 2)\n" +
	"Query Identifier: 2904850417278466856\n" +
	"Sample Scan on public.o  (cost=0.00..59.00 rows=500 width=53)\n" +
	"  Output: id, status, amount, created, note, tags\n" +
	"  Sampling: bernoulli ('10'::real) REPEATABLE ('42'::double precision)\n" +
	"Query Identifier: -1760397667436510103\n" +
	"WindowAgg  (cost=0.34..271.28 rows=5000 width=12)\n" +
	"  Output: o.id, row_number() OVER w1\n" +
	"  Window: w1 AS (ORDER BY o.id ROWS UNBOUNDED PRECEDING)\n" +
	"  Run Condition: (row_number() OVER w1 <= 10)\n" +
	"  ->  Index Only Scan using o_pkey on public.o  (cost=0.28..196.28 rows=5000 width=4)\n" +
	"        Output: o.id\n" +
	"Query Identifier: -6534064770539405473\n" +
	"Seq Scan on public.o  (cost=385.14..501.64 rows=2500 width=53)\n" +
	"  Output: o.id, o.status, o.amount, o.created, o.note, o.tags\n" +
	"  Filter: (NOT (ANY (o.id = (hashed SubPlan 1).col1)))\n" +
	"  SubPlan 1\n" +
	"    ->  Seq Scan on public.li  (cost=0.00..378.00 rows=2857 width=4)\n" +
	"          Output: li.o_id\n" +
	"          Filter: (li.qty = 6)\n" +
	"Query Identifier: 6528788463432855267\n" +
	"Seq Scan on public.o  (cost=124.85..241.35 rows=1667 width=53)\n" +
	"  Output: o.id, o.status, o.amount, o.created, o.note, o.tags\n" +
	"  Filter: (o.amount > (InitPlan 1).col1)\n" +
	"  InitPlan 1\n" +
	"    ->  Aggregate  (cost=124.84..124.85 rows=1 width=32)\n" +
	"          Output: avg(o_1.amount)\n" +
	"          ->  Seq Scan on public.o o_1  (cost=0.00..116.50 rows=3334 width=6)\n" +
	"                Output: o_1.id, o_1.status, o_1.amount, o_1.created, o_1.note, o_1.tags\n" +
	"                Filter: ((o_1.status)::text = 'paid'::text)\n" +
	"Query Identifier: -8820146289253743602\n" +
	"Nested Loop  (cost=0.29..1370.95 rows=2856 width=71)\n" +
	"  Output: li.id, li.o_id, li.sku, li.qty, o.id, o.status, o.amount, o.created, o.note, o.tags\n" +
	"  Inner Unique: true\n" +
	"  ->  Seq Scan on public.li  (cost=0.00..378.00 rows=2857 width=18)\n" +
	"        Output: li.id, li.o_id, li.sku, li.qty\n" +
	"        Filter: (li.qty = 2)\n" +
	"  ->  Memoize  (cost=0.29..0.41 rows=1 width=53)\n" +
	"        Output: o.id, o.status, o.amount, o.created, o.note, o.tags\n" +
	"        Cache Key: li.o_id\n" +
	"        Cache Mode: logical\n" +
	"        ->  Index Scan using o_pkey on public.o  (cost=0.28..0.40 rows=1 width=53)\n" +
	"              Output: o.id, o.status, o.amount, o.created, o.note, o.tags\n" +
	"              Index Cond: (o.id = li.o_id)\n" +
	"              Filter: (o.note <> 'x'::text)\n" +
	"Settings: enable_hashjoin = 'off', enable_mergejoin = 'off'\n" +
	"Query Identifier: 4593441684941084786\n" +
	"Limit  (cost=0.33..0.75 rows=5 width=53)\n" +
	"  Output: id, status, amount, created, note, tags\n" +
	"  ->  Incremental Sort  (cost=0.33..421.28 rows=5000 width=53)\n" +
	"        Output: id, status, amount, created, note, tags\n" +
	"        Sort Key: o.created, o.note\n" +
	"        Presorted Key: o.created\n" +
	"        ->  Index Scan using o_created on public.o  (cost=0.28..196.28 rows=5000 width=53)\n" +
	"              Output: id, status, amount, created, note, tags\n" +
	"Query Identifier: 5347515783860704991\n" +
	"Insert on public.o  (cost=0.00..0.01 rows=0 width=0)\n" +
	"  Conflict Resolution: UPDATE\n" +
	"  Conflict Arbiter Indexes: o_pkey\n" +
	"  Conflict Filter: ((o.status)::text <> 'locked'::text)\n" +
	"  ->  Result  (cost=0.00..0.01 rows=1 width=150)\n" +
	"        Output: 1, 'new'::character varying(20), NULL::numeric(10,2), NULL::timestamp without time zone, NULL::text, NULL::integer[]\n" +
	"Query Identifier: 279310980255717666\n" +
	"Index Scan using o_pkey on public.o  (cost=0.28..8.30 rows=1 width=53)\n" +
	"  Output: id, status, amount, created, note, tags\n" +
	"  Index Cond: (o.id = $1)\n" +
	"  Filter: ((o.status)::text = $2)\n" +
	"Settings: plan_cache_mode = 'force_generic_plan'\n" +
	"Query Identifier: 633526307822385766\n" +
	"Index Scan using o_pkey on public.o  (cost=0.28..8.30 rows=1 width=53)\n" +
	"  Output: id, status, amount, created, note, tags\n" +
	"  Index Cond: (o.id = 42)\n" +
	"  Filter: ((o.status)::text = 'pending'::text)\n" +
	"Settings: plan_cache_mode = 'force_custom_plan'\n" +
	"Query Identifier: 633526307822385766\n"

func TestRedactLiterals(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		want string
	}{
		// measured on postgres:18, 2026-09-27
		{
			name: "numbers and strings, each type kept",
			in:   `((o.amount > 99.50) AND (o.created > '2026-02-01 00:00:00'::timestamp without time zone) AND (o.note ~~ 'n1%'::text) AND (o.id <> '-5'::integer))`,
			want: `((o.amount > <redacted>) AND (o.created > '<redacted>'::timestamp without time zone) AND (o.note ~~ '<redacted>'::text) AND (o.id <> '<redacted>'::integer))`,
		},
		{
			name: "a type's modifiers are not values",
			in:   `id, (status)::character varying(10), (amount)::numeric(12,3)`,
			want: `id, (status)::character varying(10), (amount)::numeric(12,3)`,
		},
		{
			name: "an array literal",
			in:   `((o.status)::text = ANY ('{pending,void}'::text[]))`,
			want: `((o.status)::text = ANY ('<redacted>'::text[]))`,
		},
		{
			name: "SubPlan and InitPlan keep their numbers",
			in:   `(NOT (ANY (o.id = (hashed SubPlan 1).col1))) AND (o.amount > (InitPlan 1).col1)`,
			want: `(NOT (ANY (o.id = (hashed SubPlan 1).col1))) AND (o.amount > (InitPlan 1).col1)`,
		},
		{
			name: "a window's run condition, its name kept",
			in:   `(row_number() OVER w1 <= 10)`,
			want: `(row_number() OVER w1 <= <redacted>)`,
		},
		{
			name: "a function's arguments",
			in:   `generate_series(1, 10, 2)`,
			want: `generate_series(<redacted>, <redacted>, <redacted>)`,
		},
		{
			name: "a sample's arguments",
			in:   `bernoulli ('10'::real) REPEATABLE ('42'::double precision)`,
			want: `bernoulli ('<redacted>'::real) REPEATABLE ('<redacted>'::double precision)`,
		},
		{
			name: "a constant row, NULLs kept",
			in:   `1, 'new'::character varying(20), NULL::numeric(10,2), NULL::timestamp without time zone, NULL::text, NULL::integer[]`,
			want: `<redacted>, '<redacted>'::character varying(20), NULL::numeric(10,2), NULL::timestamp without time zone, NULL::text, NULL::integer[]`,
		},
		{
			name: "parameters and keywords are not values",
			in:   `((o.id = $1) AND ((o.status)::text = $2) AND flag = true AND x IS NULL)`,
			want: `((o.id = $1) AND ((o.status)::text = $2) AND flag = true AND x IS NULL)`,
		},

		// constructed, one per kind of literal the lexer knows
		{name: "a doubled quote", in: `'it''s'`, want: `'<redacted>'`},
		{name: "an escape string", in: `E'a\'b' || e'\\'`, want: `E'<redacted>' || e'<redacted>'`},
		{name: "a dollar-quoted string", in: `$$a 'b'$$ || $tag$c$tag$`, want: `$$<redacted>$$ || $tag$<redacted>$tag$`},
		{name: "prefixed strings", in: `U&'d\0061t' || B'1010' || X'1F' || N'n'`, want: `U&'<redacted>' || B'<redacted>' || X'<redacted>' || N'<redacted>'`},
		{name: "numbers of every form", in: `1_000 + 1.5e-3 + .5 + 0x1F`, want: `<redacted> + <redacted> + <redacted> + <redacted>`},
		{name: "a subscript", in: `(o.tags[1] = 4)`, want: `(o.tags[<redacted>] = <redacted>)`},
		{name: "digits inside names", in: `t1.col2 = "Col 1"`, want: `t1.col2 = "Col 1"`},
		{name: "a quoted type", in: `x::"My Type"(3)`, want: `x::"My Type"(3)`},
		{name: "comments stay as written", in: "SELECT /* id=5 */ 1 -- 'x'\nFROM t", want: "SELECT /* id=5 */ <redacted> -- 'x'\nFROM t"},
		{name: "a quote left open runs to the end", in: `x = 'abc`, want: `x = '<redacted>'`},

		// measured on postgres:18, 2026-09-27: a Query Text over four lines
		{
			name: "SQL keeps its shape and its line breaks",
			in:   "SELECT count(*)\n  FROM o\n WHERE note = 'multi line plan secret'\n   AND id > 7;",
			want: "SELECT count(*)\n  FROM o\n WHERE note = '<redacted>'\n   AND id > <redacted>;",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, redacted := redactLiterals(tt.in)

			assert.Equal(t, tt.want, got)
			assert.Equal(t, strings.Count(tt.want, redactedValue), redacted)
		})
	}
}

func TestRedactPlanKeepsEveryLineButTheExpressions(t *testing.T) {
	got, redacted := redactPlan(measuredVerbosePlans)

	in := strings.Split(measuredVerbosePlans, "\n")
	out := strings.Split(got, "\n")
	require.Len(t, out, len(in), "a text plan keeps its lines")

	changed := 0

	for i := range in {
		if in[i] == out[i] {
			continue
		}

		changed++

		require.True(t, textPlanProperty.MatchString(in[i]),
			"only an expression's line changes, never a node's costs and rows, a setting or an identifier: %q", in[i])
	}

	assert.Equal(t, 22, changed)
	assert.Equal(t, 31, redacted)

	for _, value := range []string{"pending", "paid", "99.50", "sku-7", "2026-02-01", "(0,1)", "'x'", "locked", "'new'", "= 42", "3.00"} {
		assert.NotContains(t, got, value)
	}

	for _, kept := range []string{
		"Bitmap Heap Scan on public.o  (cost=24.86..117.90 rows=311 width=58)",
		"Recheck Cond: ((o.status)::text = '<redacted>'::text)",
		"Index Cond: (o.id = $1)",
		"Index Cond: (o.id = <redacted>)",
		"Settings: plan_cache_mode = 'force_custom_plan'",
		"Settings: enable_hashjoin = 'off', enable_nestloop = 'off'",
		"Query Identifier: 633526307822385766",
		"Filter: (NOT (ANY (o.id = (hashed SubPlan 1).col1)))",
		"Conflict Arbiter Indexes: o_pkey",
		"Cache Mode: logical",
	} {
		assert.Contains(t, got, kept)
	}
}

func TestLoggedPlanRedactionInEveryLogAndPlanFormat(t *testing.T) {
	for _, stream := range []struct {
		format logFormat
		log    string
	}{
		{logFormatStderr, measuredAutoExplain},
		{logFormatCSV, measuredAutoExplainCSV},
		{logFormatJSON, measuredAutoExplainJSON},
	} {
		t.Run(string(stream.format), func(t *testing.T) {
			events, _, _, matched := matchEvents([]byte(ended(stream.format, stream.log)), stream.format,
				explainMatch, &tailRead{})
			require.Equal(t, 8, matched, "two statements in four plan formats")

			var body strings.Builder

			total := 0

			for _, event := range events {
				got, redacted := loggedPlanRedaction.event(event, stream.format)
				assert.Contains(t, []int{4, 7}, redacted,
					"seven in the first statement, four in the second, whatever the plan's format: %s", got)

				body.Write(got)
				total += redacted
			}

			assert.Equal(t, 44, total)

			written := body.String()

			for _, value := range []string{"secret", "pending", "99.50", "> 7", "&gt; 7"} {
				assert.NotContains(t, written, value)
			}

			for _, kept := range []string{
				"duration: 0.434 ms  plan:", "Bitmap Heap Scan", "o_status", "(cost=25.19..108.34 rows=1644 width=10)",
				"Startup Cost", "25.19", "Plan-Rows>1644</Plan-Rows>", "Query Identifier", "5561098070303724986",
				"$1 = '<redacted>'", "status = $1",
			} {
				assert.Contains(t, written, kept)
			}
		})
	}
}

func TestLoggedPlanRedactionKeepsEachPlanFormatsOwnEncoding(t *testing.T) {
	entries, _, _, _ := matchEvents([]byte(measuredAutoExplain+unrelatedTraffic), logFormatStderr, explainMatch, &tailRead{})

	for _, tt := range []struct {
		name string
		find string
		want string
	}{
		{"text", "\tQuery Parameters: $1 = 'pending'\n", "\tQuery Parameters: $1 = '<redacted>'\n"},
		{"json", "\t    \"Filter\": \"((o.amount > 99.50) AND (o.note <> 'plan secret'::text))\",\n",
			"\t    \"Filter\": \"((o.amount > <redacted>) AND (o.note <> '<redacted>'::text))\",\n"},
		{"json list", "\t    \"Output\": [\"id\", \"amount\"],\n", "\t    \"Output\": [\"id\", \"amount\"],\n"},
		// The placeholder is text in the element, so it is escaped as the server escapes one.
		{"xml", "\t    <Filter>((o.amount &gt; 99.50) AND (o.note &lt;&gt; 'plan secret'::text))</Filter>\n",
			"\t    <Filter>((o.amount &gt; &lt;redacted&gt;) AND (o.note &lt;&gt; '&lt;redacted&gt;'::text))</Filter>\n"},
		{"xml over lines", "\t   AND id &gt; 7;</Query-Text>\n", "\t   AND id &gt; &lt;redacted&gt;;</Query-Text>\n"},
		{"yaml", "\t  Filter: \"((o.amount > 99.50) AND (o.note <> 'plan secret'::text))\"\n",
			"\t  Filter: \"((o.amount > <redacted>) AND (o.note <> '<redacted>'::text))\"\n"},
		{"yaml escapes", "\tQuery Text: \"SELECT count(*)\\n  FROM o\\n WHERE note = 'multi line plan secret'\\n   AND id > 7;\"\n",
			"\tQuery Text: \"SELECT count(*)\\n  FROM o\\n WHERE note = '<redacted>'\\n   AND id > <redacted>;\"\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var entry string

			for _, e := range entries {
				if strings.Contains(string(e), tt.find) {
					entry = string(e)
				}
			}

			require.NotEmpty(t, entry, "the measured entries hold %q", tt.find)

			got, _ := loggedPlanRedaction.event([]byte(entry), logFormatStderr)
			assert.Contains(t, string(got), tt.want)
		})
	}
}

func TestRedactErrorText(t *testing.T) {
	got, redacted := redactErrorText(`ERROR: invalid input syntax for type integer: "abc" (SQLSTATE 22P02)`)
	assert.Equal(t, `ERROR: invalid input syntax for type integer: "<redacted>" (SQLSTATE 22P02)`, got)
	assert.Equal(t, 1, redacted)

	got, redacted = redactErrorText("ERROR: permission denied for table orders")
	assert.Equal(t, "ERROR: permission denied for table orders", got, "a name, and no code in the text")
	assert.Zero(t, redacted)
}

func TestExplainBlocksCountWhatTheyReplaced(t *testing.T) {
	e := NewExplain(ExplainModeAll, itemsFeed())

	var out bytes.Buffer

	literal := &explainCandidate{
		queryid: ptr(int64(-4821096637582910234)), firstSeen: 1, mode: planModeEstimatedLiteral,
		plan: []byte(" Seq Scan on public.order_items  (cost=0.00..8420.00 rows=3 width=64)\n" +
			"   Filter: (order_items.order_id = 4021)\n"),
	}
	refused := &explainCandidate{
		queryid: ptr(int64(5548219003471002234)), firstSeen: 1, mode: planModeNone,
		err: `ERROR: invalid input syntax for type integer: "abc" (SQLSTATE 22P02)`,
	}

	s := SampleContext{Index: 1, Total: 2, Database: "orders_db", DBID: "16401", At: time.Now()}

	require.NoError(t, e.writeCandidate(&out, s, literal, explainFacts{}))
	require.NoError(t, e.writeCandidate(&out, s, refused, explainFacts{}))

	written := out.String()

	assert.Contains(t, written, "redacted=1 bytes=")
	assert.Contains(t, written, "Filter: (order_items.order_id = <redacted>)")
	assert.Contains(t, written, `error="ERROR: invalid input syntax for type integer: \"<redacted>\" (SQLSTATE 22P02)" redacted=1`,
		"the literal tier's error quoting a value from the log is redacted too")
	assert.NotContains(t, written, "4021")
	assert.NotContains(t, written, "abc")
}
