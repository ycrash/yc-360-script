package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// measured on postgres:18, 2026-09-27, one session per event, with log_destination set to
// stderr,csvlog,jsonlog so the three files hold the same events.
const (
	measuredUniqueViolation = "2026-09-27 01:49:46.564 UTC [12395] ERROR:  duplicate key value violates unique constraint \"yc_errs_orders_pkey\"\n" +
		"2026-09-27 01:49:46.564 UTC [12395] DETAIL:  Key (id)=(4021) already exists.\n" +
		"2026-09-27 01:49:46.564 UTC [12395] STATEMENT:  INSERT INTO yc_errs_orders (id, status) VALUES (4021, 'pending');\n"

	measuredWarning = "2026-09-27 01:49:46.565 UTC [12395] WARNING:  yc-360 errors matcher sample warning\n" +
		"2026-09-27 01:49:46.565 UTC [12395] CONTEXT:  PL/pgSQL function inline_code_block line 1 at RAISE\n"

	measuredTimeoutBeside = "2026-09-27 01:49:46.870 UTC [12395] ERROR:  canceling statement due to statement timeout\n" +
		"2026-09-27 01:49:46.870 UTC [12395] STATEMENT:  SELECT pg_sleep(2);\n"

	measuredMissingDatabase = "2026-09-27 01:49:46.933 UTC [12402] FATAL:  database \"yc_no_such_database\" does not exist\n"

	measuredUserCancel = "2026-09-27 01:50:00.075 UTC [12463] ERROR:  canceling statement due to user request\n" +
		"2026-09-27 01:50:00.075 UTC [12463] STATEMENT:  SELECT pg_sleep(5)\n"

	measuredNowait = "2026-09-27 01:50:01.159 UTC [12492] ERROR:  could not obtain lock on row in relation \"yc_errs_lock\"\n" +
		"2026-09-27 01:50:01.159 UTC [12492] STATEMENT:  SELECT id FROM yc_errs_lock WHERE id = 1 FOR UPDATE NOWAIT\n"

	measuredReload = "2026-09-27 01:50:03.232 UTC [1] LOG:  received SIGHUP, reloading configuration files\n" +
		"2026-09-27 01:50:03.232 UTC [1] LOG:  parameter \"log_destination\" removed from configuration file, reset to default\n"
)

const (
	measuredUniqueViolationCSV = `2026-09-27 01:49:46.564 UTC,"postgres","postgres",12395,"[local]",6ab8763a.306b,1,"INSERT",2026-09-27 01:49:46 UTC,21/620,2077,ERROR,23505,"duplicate key value violates unique constraint ""yc_errs_orders_pkey""","Key (id)=(4021) already exists.",,,,,"INSERT INTO yc_errs_orders (id, status) VALUES (4021, 'pending');",,,"psql","client backend",,8939275448745717619` + "\n"

	measuredWarningCSV = `2026-09-27 01:49:46.565 UTC,"postgres","postgres",12395,"[local]",6ab8763a.306b,2,"DO",2026-09-27 01:49:46 UTC,21/621,0,WARNING,01000,"yc-360 errors matcher sample warning",,,,,"PL/pgSQL function inline_code_block line 1 at RAISE",,,,"psql","client backend",,6083687453455777963` + "\n"

	measuredTimeoutBesideCSV = `2026-09-27 01:49:46.870 UTC,"postgres","postgres",12395,"[local]",6ab8763a.306b,3,"SELECT",2026-09-27 01:49:46 UTC,21/623,0,ERROR,57014,"canceling statement due to statement timeout",,,,,,"SELECT pg_sleep(2);",,,"psql","client backend",,-8886085649470066503` + "\n"

	measuredMissingDatabaseCSV = `2026-09-27 01:49:46.933 UTC,"postgres","yc_no_such_database",12402,"[local]",6ab8763a.3072,1,"startup",2026-09-27 01:49:46 UTC,24/444,0,FATAL,3D000,"database ""yc_no_such_database"" does not exist",,,,,,,,,"","client backend",,0` + "\n"

	measuredUserCancelCSV = `2026-09-27 01:50:00.075 UTC,"postgres","postgres",12463,"[local]",6ab87647.30af,1,"SELECT",2026-09-27 01:49:59 UTC,12/718,0,ERROR,57014,"canceling statement due to user request",,,,,,"SELECT pg_sleep(5)",,,"yc_errs_cancel","client backend",,-8886085649470066503` + "\n"

	measuredNowaitCSV = `2026-09-27 01:50:01.159 UTC,"postgres","postgres",12492,"[local]",6ab87649.30cc,1,"SELECT",2026-09-27 01:50:01 UTC,53/414,0,ERROR,55P03,"could not obtain lock on row in relation ""yc_errs_lock""",,,,,,"SELECT id FROM yc_errs_lock WHERE id = 1 FOR UPDATE NOWAIT",,,"psql","client backend",,1217037425451197254` + "\n"

	measuredReloadCSV = `2026-09-27 01:50:03.232 UTC,,,1,,6ab73ece.1,50,,2026-09-26 03:41:02 UTC,,0,LOG,00000,"received SIGHUP, reloading configuration files",,,,,,,,,"","postmaster",,0` + "\n"
)

const (
	measuredUniqueViolationJSON = `{"timestamp":"2026-09-27 01:49:46.564 UTC","user":"postgres","dbname":"postgres","pid":12395,"remote_host":"[local]","session_id":"6ab8763a.306b","line_num":1,"ps":"INSERT","session_start":"2026-09-27 01:49:46 UTC","vxid":"21/620","txid":2077,"error_severity":"ERROR","state_code":"23505","message":"duplicate key value violates unique constraint \"yc_errs_orders_pkey\"","detail":"Key (id)=(4021) already exists.","statement":"INSERT INTO yc_errs_orders (id, status) VALUES (4021, 'pending');","application_name":"psql","backend_type":"client backend","query_id":8939275448745717619}` + "\n"

	measuredWarningJSON = `{"timestamp":"2026-09-27 01:49:46.565 UTC","user":"postgres","dbname":"postgres","pid":12395,"remote_host":"[local]","session_id":"6ab8763a.306b","line_num":2,"ps":"DO","session_start":"2026-09-27 01:49:46 UTC","vxid":"21/621","txid":0,"error_severity":"WARNING","state_code":"01000","message":"yc-360 errors matcher sample warning","context":"PL/pgSQL function inline_code_block line 1 at RAISE","application_name":"psql","backend_type":"client backend","query_id":6083687453455777963}` + "\n"

	measuredTimeoutBesideJSON = `{"timestamp":"2026-09-27 01:49:46.870 UTC","user":"postgres","dbname":"postgres","pid":12395,"remote_host":"[local]","session_id":"6ab8763a.306b","line_num":3,"ps":"SELECT","session_start":"2026-09-27 01:49:46 UTC","vxid":"21/623","txid":0,"error_severity":"ERROR","state_code":"57014","message":"canceling statement due to statement timeout","statement":"SELECT pg_sleep(2);","application_name":"psql","backend_type":"client backend","query_id":-8886085649470066503}` + "\n"

	measuredMissingDatabaseJSON = `{"timestamp":"2026-09-27 01:49:46.933 UTC","user":"postgres","dbname":"yc_no_such_database","pid":12402,"remote_host":"[local]","session_id":"6ab8763a.3072","line_num":1,"ps":"startup","session_start":"2026-09-27 01:49:46 UTC","vxid":"24/444","txid":0,"error_severity":"FATAL","state_code":"3D000","message":"database \"yc_no_such_database\" does not exist","backend_type":"client backend","query_id":0}` + "\n"

	measuredUserCancelJSON = `{"timestamp":"2026-09-27 01:50:00.075 UTC","user":"postgres","dbname":"postgres","pid":12463,"remote_host":"[local]","session_id":"6ab87647.30af","line_num":1,"ps":"SELECT","session_start":"2026-09-27 01:49:59 UTC","vxid":"12/718","txid":0,"error_severity":"ERROR","state_code":"57014","message":"canceling statement due to user request","statement":"SELECT pg_sleep(5)","application_name":"yc_errs_cancel","backend_type":"client backend","query_id":-8886085649470066503}` + "\n"

	measuredNowaitJSON = `{"timestamp":"2026-09-27 01:50:01.159 UTC","user":"postgres","dbname":"postgres","pid":12492,"remote_host":"[local]","session_id":"6ab87649.30cc","line_num":1,"ps":"SELECT","session_start":"2026-09-27 01:50:01 UTC","vxid":"53/414","txid":0,"error_severity":"ERROR","state_code":"55P03","message":"could not obtain lock on row in relation \"yc_errs_lock\"","statement":"SELECT id FROM yc_errs_lock WHERE id = 1 FOR UPDATE NOWAIT","application_name":"psql","backend_type":"client backend","query_id":1217037425451197254}` + "\n"

	measuredReloadJSON = `{"timestamp":"2026-09-27 01:50:03.232 UTC","pid":1,"session_id":"6ab73ece.1","line_num":50,"session_start":"2026-09-26 03:41:02 UTC","txid":0,"error_severity":"LOG","message":"received SIGHUP, reloading configuration files","backend_type":"postmaster","query_id":0}` + "\n"
)

// measured on postgres:18, 2026-09-27, one database, with log_destination set to
// stderr,csvlog,jsonlog: errors that quote values in every field that can, each of
// the known shapes once, and an application's own RAISE. The values all say "secret".
const (
	measuredValueErrors = "2026-09-27 04:35:12.093 UTC [52514] ERROR:  duplicate key value violates unique constraint \"p_pkey\"\n" +
		"2026-09-27 04:35:12.093 UTC [52514] DETAIL:  Key (id)=(1) already exists.\n" +
		"2026-09-27 04:35:12.093 UTC [52514] STATEMENT:  INSERT INTO p VALUES (1);\n" +
		"2026-09-27 04:35:12.094 UTC [52514] ERROR:  insert or update on table \"c\" violates foreign key constraint \"c_pid_fkey\"\n" +
		"2026-09-27 04:35:12.094 UTC [52514] DETAIL:  Key (pid)=(99) is not present in table \"p\".\n" +
		"2026-09-27 04:35:12.094 UTC [52514] STATEMENT:  INSERT INTO c VALUES (1, 99, 'fk secret', 5);\n" +
		"2026-09-27 04:35:12.094 UTC [52514] ERROR:  new row for relation \"c\" violates check constraint \"c_amount_check\"\n" +
		"2026-09-27 04:35:12.094 UTC [52514] DETAIL:  Failing row contains (2, 1, check secret, -5).\n" +
		"2026-09-27 04:35:12.094 UTC [52514] STATEMENT:  INSERT INTO c VALUES (2, 1, 'check secret', -5);\n" +
		"2026-09-27 04:35:12.094 UTC [52514] ERROR:  null value in column \"note\" of relation \"c\" violates not-null constraint\n" +
		"2026-09-27 04:35:12.094 UTC [52514] DETAIL:  Failing row contains (3, 1, null, 5).\n" +
		"2026-09-27 04:35:12.094 UTC [52514] STATEMENT:  INSERT INTO c VALUES (3, 1, NULL, 5);\n" +
		"2026-09-27 04:35:12.094 UTC [52514] ERROR:  invalid input syntax for type integer: \"abc secret\" at character 8\n" +
		"2026-09-27 04:35:12.094 UTC [52514] STATEMENT:  SELECT 'abc secret'::int;\n" +
		"2026-09-27 04:35:12.094 UTC [52514] ERROR:  invalid input syntax for type json at character 8\n" +
		"2026-09-27 04:35:12.094 UTC [52514] DETAIL:  Token \"secret\" is invalid.\n" +
		"2026-09-27 04:35:12.094 UTC [52514] CONTEXT:  JSON data, line 1: {\"card\": 4111 secret...\n" +
		"2026-09-27 04:35:12.094 UTC [52514] STATEMENT:  SELECT '{\"card\": 4111 secret}'::json;\n" +
		"2026-09-27 04:35:12.094 UTC [52514] ERROR:  invalid input syntax for type integer: \"multi line secret\" at character 13\n" +
		"2026-09-27 04:35:12.094 UTC [52514] STATEMENT:  SELECT 1,\n" +
		"\t  'multi line secret'::int;\n" +
		"2026-09-27 04:35:12.095 UTC [52514] ERROR:  customer 12345 over limit\n" +
		"2026-09-27 04:35:12.095 UTC [52514] DETAIL:  balance 99.50\n" +
		"2026-09-27 04:35:12.095 UTC [52514] HINT:  call 555-0100\n" +
		"2026-09-27 04:35:12.095 UTC [52514] CONTEXT:  PL/pgSQL function inline_code_block line 1 at RAISE\n" +
		"2026-09-27 04:35:12.095 UTC [52514] STATEMENT:  DO $$ BEGIN RAISE EXCEPTION 'customer % over limit', 12345 USING DETAIL = 'balance 99.50', HINT = 'call 555-0100'; END $$;\n" +
		"2026-09-27 04:35:12.095 UTC [52514] ERROR:  syntax error at or near \"SELEC\" at character 1\n" +
		"2026-09-27 04:35:12.095 UTC [52514] QUERY:  SELEC 'exec secret'\n" +
		"2026-09-27 04:35:12.095 UTC [52514] CONTEXT:  PL/pgSQL function inline_code_block line 1 at EXECUTE\n" +
		"2026-09-27 04:35:12.095 UTC [52514] STATEMENT:  DO $$ BEGIN EXECUTE 'SELEC ''exec secret'''; END $$;\n" +
		"2026-09-27 04:35:12.095 UTC [52514] ERROR:  duplicate key value violates unique constraint \"p_pkey\"\n" +
		"2026-09-27 04:35:12.095 UTC [52514] DETAIL:  Key (id)=(1) already exists.\n" +
		"2026-09-27 04:35:12.095 UTC [52514] CONTEXT:  SQL statement \"INSERT INTO p VALUES (1)\"\n" +
		"\tPL/pgSQL function inline_code_block line 1 at SQL statement\n" +
		"2026-09-27 04:35:12.095 UTC [52514] STATEMENT:  DO $$ BEGIN INSERT INTO p VALUES (1); END $$;\n" +
		"2026-09-27 04:35:12.095 UTC [52514] ERROR:  23505: duplicate key value violates unique constraint \"p_pkey\"\n" +
		"2026-09-27 04:35:12.095 UTC [52514] DETAIL:  Key (id)=(1) already exists.\n" +
		"2026-09-27 04:35:12.095 UTC [52514] LOCATION:  _bt_check_unique, nbtinsert.c:666\n" +
		"2026-09-27 04:35:12.095 UTC [52514] STATEMENT:  INSERT INTO p VALUES (1);\n" +
		"2026-09-27 04:35:12.178 UTC [52521] ERROR:  invalid input syntax for type integer: \"x secret\"\n" +
		"2026-09-27 04:35:12.178 UTC [52521] CONTEXT:  COPY c, line 2, column id: \"x secret\"\n" +
		"2026-09-27 04:35:12.178 UTC [52521] STATEMENT:  COPY c FROM stdin\n" +
		"2026-09-27 04:35:12.232 UTC [52536] ERROR:  new row for relation \"c\" violates check constraint \"c_amount_check\"\n" +
		"2026-09-27 04:35:12.232 UTC [52536] DETAIL:  Failing row contains (11, 1, bad, -7).\n" +
		"2026-09-27 04:35:12.232 UTC [52536] CONTEXT:  COPY c, line 2: \"11\t1\tbad\t-7\"\n" +
		"2026-09-27 04:35:12.232 UTC [52536] STATEMENT:  COPY c FROM stdin\n" +
		"2026-09-27 04:35:12.299 UTC [52543] ERROR:  invalid input syntax for type integer: \"bind secret\"\n" +
		"2026-09-27 04:35:12.299 UTC [52543] CONTEXT:  unnamed portal parameter $1 = 'bind secret'\n" +
		"2026-09-27 04:35:12.299 UTC [52543] STATEMENT:  SELECT $1::int \n" +
		"2026-09-27 04:35:12.300 UTC [52543] ERROR:  division by zero\n" +
		"2026-09-27 04:35:12.300 UTC [52543] CONTEXT:  unnamed portal with parameters: $1 = '5', $2 = 'param secret'\n" +
		"2026-09-27 04:35:12.300 UTC [52543] STATEMENT:  SELECT 1 / ($1::int - 5), $2::text \n"

	measuredValueErrorsCSV = `2026-09-27 04:35:12.093 UTC,"postgres","yc_redact_measure",52514,"[local]",6ab89d00.cd22,1,"INSERT",2026-09-27 04:35:12 UTC,84/1010,2525,ERROR,23505,"duplicate key value violates unique constraint ""p_pkey""","Key (id)=(1) already exists.",,,,,"INSERT INTO p VALUES (1);",,,"psql","client backend",,4678092893242982148` + "\n" +
		`2026-09-27 04:35:12.094 UTC,"postgres","yc_redact_measure",52514,"[local]",6ab89d00.cd22,2,"INSERT",2026-09-27 04:35:12 UTC,84/1011,2526,ERROR,23503,"insert or update on table ""c"" violates foreign key constraint ""c_pid_fkey""","Key (pid)=(99) is not present in table ""p"".",,,,,"INSERT INTO c VALUES (1, 99, 'fk secret', 5);",,,"psql","client backend",,4086326952067363150` + "\n" +
		`2026-09-27 04:35:12.094 UTC,"postgres","yc_redact_measure",52514,"[local]",6ab89d00.cd22,3,"INSERT",2026-09-27 04:35:12 UTC,84/1012,0,ERROR,23514,"new row for relation ""c"" violates check constraint ""c_amount_check""","Failing row contains (2, 1, check secret, -5).",,,,,"INSERT INTO c VALUES (2, 1, 'check secret', -5);",,,"psql","client backend",,4086326952067363150` + "\n" +
		`2026-09-27 04:35:12.094 UTC,"postgres","yc_redact_measure",52514,"[local]",6ab89d00.cd22,4,"INSERT",2026-09-27 04:35:12 UTC,84/1013,0,ERROR,23502,"null value in column ""note"" of relation ""c"" violates not-null constraint","Failing row contains (3, 1, null, 5).",,,,,"INSERT INTO c VALUES (3, 1, NULL, 5);",,,"psql","client backend",,4086326952067363150` + "\n" +
		`2026-09-27 04:35:12.094 UTC,"postgres","yc_redact_measure",52514,"[local]",6ab89d00.cd22,5,"SELECT",2026-09-27 04:35:12 UTC,84/1014,0,ERROR,22P02,"invalid input syntax for type integer: ""abc secret""",,,,,,"SELECT 'abc secret'::int;",8,,"psql","client backend",,0` + "\n" +
		`2026-09-27 04:35:12.094 UTC,"postgres","yc_redact_measure",52514,"[local]",6ab89d00.cd22,6,"SELECT",2026-09-27 04:35:12 UTC,84/1015,0,ERROR,22P02,"invalid input syntax for type json","Token ""secret"" is invalid.",,,,"JSON data, line 1: {""card"": 4111 secret...","SELECT '{""card"": 4111 secret}'::json;",8,,"psql","client backend",,0` + "\n" +
		`2026-09-27 04:35:12.094 UTC,"postgres","yc_redact_measure",52514,"[local]",6ab89d00.cd22,7,"SELECT",2026-09-27 04:35:12 UTC,84/1016,0,ERROR,22P02,"invalid input syntax for type integer: ""multi line secret""",,,,,,"SELECT 1,
  'multi line secret'::int;",13,,"psql","client backend",,0` + "\n" +
		`2026-09-27 04:35:12.095 UTC,"postgres","yc_redact_measure",52514,"[local]",6ab89d00.cd22,8,"DO",2026-09-27 04:35:12 UTC,84/1017,0,ERROR,P0001,"customer 12345 over limit","balance 99.50","call 555-0100",,,"PL/pgSQL function inline_code_block line 1 at RAISE","DO $$ BEGIN RAISE EXCEPTION 'customer % over limit', 12345 USING DETAIL = 'balance 99.50', HINT = 'call 555-0100'; END $$;",,,"psql","client backend",,5542336743836019324` + "\n" +
		`2026-09-27 04:35:12.095 UTC,"postgres","yc_redact_measure",52514,"[local]",6ab89d00.cd22,9,"DO",2026-09-27 04:35:12 UTC,84/1018,0,ERROR,42601,"syntax error at or near ""SELEC""",,,"SELEC 'exec secret'",1,"PL/pgSQL function inline_code_block line 1 at EXECUTE","DO $$ BEGIN EXECUTE 'SELEC ''exec secret'''; END $$;",,,"psql","client backend",,6375509697466515017` + "\n" +
		`2026-09-27 04:35:12.095 UTC,"postgres","yc_redact_measure",52514,"[local]",6ab89d00.cd22,10,"DO",2026-09-27 04:35:12 UTC,84/1019,2527,ERROR,23505,"duplicate key value violates unique constraint ""p_pkey""","Key (id)=(1) already exists.",,,,"SQL statement ""INSERT INTO p VALUES (1)""
PL/pgSQL function inline_code_block line 1 at SQL statement","DO $$ BEGIN INSERT INTO p VALUES (1); END $$;",,,"psql","client backend",,3123051761372624745` + "\n" +
		`2026-09-27 04:35:12.095 UTC,"postgres","yc_redact_measure",52514,"[local]",6ab89d00.cd22,11,"INSERT",2026-09-27 04:35:12 UTC,84/1021,2528,ERROR,23505,"duplicate key value violates unique constraint ""p_pkey""","Key (id)=(1) already exists.",,,,,"INSERT INTO p VALUES (1);",,"_bt_check_unique, nbtinsert.c:666","psql","client backend",,4678092893242982148` + "\n" +
		`2026-09-27 04:35:12.178 UTC,"postgres","yc_redact_measure",52521,"[local]",6ab89d00.cd29,1,"COPY",2026-09-27 04:35:12 UTC,1/532,0,ERROR,22P02,"invalid input syntax for type integer: ""x secret""",,,,,"COPY c, line 2, column id: ""x secret""","COPY c FROM stdin",,,"psql","client backend",,4886255104157021466` + "\n" +
		`2026-09-27 04:35:12.232 UTC,"postgres","yc_redact_measure",52536,"[local]",6ab89d00.cd38,1,"COPY",2026-09-27 04:35:12 UTC,95/557,0,ERROR,23514,"new row for relation ""c"" violates check constraint ""c_amount_check""","Failing row contains (11, 1, bad, -7).",,,,"COPY c, line 2: ""11	1	bad	-7""","COPY c FROM stdin",,,"psql","client backend",,4886255104157021466` + "\n" +
		`2026-09-27 04:35:12.299 UTC,"postgres","yc_redact_measure",52543,"[local]",6ab89d00.cd3f,1,"BIND",2026-09-27 04:35:12 UTC,10/531,0,ERROR,22P02,"invalid input syntax for type integer: ""bind secret""",,,,,"unnamed portal parameter $1 = 'bind secret'","SELECT $1::int ",,,"psql","client backend",,7559284449764764609` + "\n" +
		`2026-09-27 04:35:12.300 UTC,"postgres","yc_redact_measure",52543,"[local]",6ab89d00.cd3f,2,"BIND",2026-09-27 04:35:12 UTC,10/532,0,ERROR,22012,"division by zero",,,,,"unnamed portal with parameters: $1 = '5', $2 = 'param secret'","SELECT 1 / ($1::int - 5), $2::text ",,,"psql","client backend",,6336613098166984710` + "\n"

	measuredValueErrorsJSON = `{"timestamp":"2026-09-27 04:35:12.093 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52514,"remote_host":"[local]","session_id":"6ab89d00.cd22","line_num":1,"ps":"INSERT","session_start":"2026-09-27 04:35:12 UTC","vxid":"84/1010","txid":2525,"error_severity":"ERROR","state_code":"23505","message":"duplicate key value violates unique constraint \"p_pkey\"","detail":"Key (id)=(1) already exists.","statement":"INSERT INTO p VALUES (1);","application_name":"psql","backend_type":"client backend","query_id":4678092893242982148}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.094 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52514,"remote_host":"[local]","session_id":"6ab89d00.cd22","line_num":2,"ps":"INSERT","session_start":"2026-09-27 04:35:12 UTC","vxid":"84/1011","txid":2526,"error_severity":"ERROR","state_code":"23503","message":"insert or update on table \"c\" violates foreign key constraint \"c_pid_fkey\"","detail":"Key (pid)=(99) is not present in table \"p\".","statement":"INSERT INTO c VALUES (1, 99, 'fk secret', 5);","application_name":"psql","backend_type":"client backend","query_id":4086326952067363150}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.094 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52514,"remote_host":"[local]","session_id":"6ab89d00.cd22","line_num":3,"ps":"INSERT","session_start":"2026-09-27 04:35:12 UTC","vxid":"84/1012","txid":0,"error_severity":"ERROR","state_code":"23514","message":"new row for relation \"c\" violates check constraint \"c_amount_check\"","detail":"Failing row contains (2, 1, check secret, -5).","statement":"INSERT INTO c VALUES (2, 1, 'check secret', -5);","application_name":"psql","backend_type":"client backend","query_id":4086326952067363150}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.094 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52514,"remote_host":"[local]","session_id":"6ab89d00.cd22","line_num":4,"ps":"INSERT","session_start":"2026-09-27 04:35:12 UTC","vxid":"84/1013","txid":0,"error_severity":"ERROR","state_code":"23502","message":"null value in column \"note\" of relation \"c\" violates not-null constraint","detail":"Failing row contains (3, 1, null, 5).","statement":"INSERT INTO c VALUES (3, 1, NULL, 5);","application_name":"psql","backend_type":"client backend","query_id":4086326952067363150}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.094 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52514,"remote_host":"[local]","session_id":"6ab89d00.cd22","line_num":5,"ps":"SELECT","session_start":"2026-09-27 04:35:12 UTC","vxid":"84/1014","txid":0,"error_severity":"ERROR","state_code":"22P02","message":"invalid input syntax for type integer: \"abc secret\"","statement":"SELECT 'abc secret'::int;","cursor_position":8,"application_name":"psql","backend_type":"client backend","query_id":0}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.094 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52514,"remote_host":"[local]","session_id":"6ab89d00.cd22","line_num":6,"ps":"SELECT","session_start":"2026-09-27 04:35:12 UTC","vxid":"84/1015","txid":0,"error_severity":"ERROR","state_code":"22P02","message":"invalid input syntax for type json","detail":"Token \"secret\" is invalid.","context":"JSON data, line 1: {\"card\": 4111 secret...","statement":"SELECT '{\"card\": 4111 secret}'::json;","cursor_position":8,"application_name":"psql","backend_type":"client backend","query_id":0}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.094 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52514,"remote_host":"[local]","session_id":"6ab89d00.cd22","line_num":7,"ps":"SELECT","session_start":"2026-09-27 04:35:12 UTC","vxid":"84/1016","txid":0,"error_severity":"ERROR","state_code":"22P02","message":"invalid input syntax for type integer: \"multi line secret\"","statement":"SELECT 1,\n  'multi line secret'::int;","cursor_position":13,"application_name":"psql","backend_type":"client backend","query_id":0}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.095 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52514,"remote_host":"[local]","session_id":"6ab89d00.cd22","line_num":8,"ps":"DO","session_start":"2026-09-27 04:35:12 UTC","vxid":"84/1017","txid":0,"error_severity":"ERROR","state_code":"P0001","message":"customer 12345 over limit","detail":"balance 99.50","hint":"call 555-0100","context":"PL/pgSQL function inline_code_block line 1 at RAISE","statement":"DO $$ BEGIN RAISE EXCEPTION 'customer % over limit', 12345 USING DETAIL = 'balance 99.50', HINT = 'call 555-0100'; END $$;","application_name":"psql","backend_type":"client backend","query_id":5542336743836019324}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.095 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52514,"remote_host":"[local]","session_id":"6ab89d00.cd22","line_num":9,"ps":"DO","session_start":"2026-09-27 04:35:12 UTC","vxid":"84/1018","txid":0,"error_severity":"ERROR","state_code":"42601","message":"syntax error at or near \"SELEC\"","internal_query":"SELEC 'exec secret'","internal_position":1,"context":"PL/pgSQL function inline_code_block line 1 at EXECUTE","statement":"DO $$ BEGIN EXECUTE 'SELEC ''exec secret'''; END $$;","application_name":"psql","backend_type":"client backend","query_id":6375509697466515017}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.095 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52514,"remote_host":"[local]","session_id":"6ab89d00.cd22","line_num":10,"ps":"DO","session_start":"2026-09-27 04:35:12 UTC","vxid":"84/1019","txid":2527,"error_severity":"ERROR","state_code":"23505","message":"duplicate key value violates unique constraint \"p_pkey\"","detail":"Key (id)=(1) already exists.","context":"SQL statement \"INSERT INTO p VALUES (1)\"\nPL/pgSQL function inline_code_block line 1 at SQL statement","statement":"DO $$ BEGIN INSERT INTO p VALUES (1); END $$;","application_name":"psql","backend_type":"client backend","query_id":3123051761372624745}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.095 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52514,"remote_host":"[local]","session_id":"6ab89d00.cd22","line_num":11,"ps":"INSERT","session_start":"2026-09-27 04:35:12 UTC","vxid":"84/1021","txid":2528,"error_severity":"ERROR","state_code":"23505","message":"duplicate key value violates unique constraint \"p_pkey\"","detail":"Key (id)=(1) already exists.","statement":"INSERT INTO p VALUES (1);","func_name":"_bt_check_unique","file_name":"nbtinsert.c","file_line_num":666,"application_name":"psql","backend_type":"client backend","query_id":4678092893242982148}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.178 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52521,"remote_host":"[local]","session_id":"6ab89d00.cd29","line_num":1,"ps":"COPY","session_start":"2026-09-27 04:35:12 UTC","vxid":"1/532","txid":0,"error_severity":"ERROR","state_code":"22P02","message":"invalid input syntax for type integer: \"x secret\"","context":"COPY c, line 2, column id: \"x secret\"","statement":"COPY c FROM stdin","application_name":"psql","backend_type":"client backend","query_id":4886255104157021466}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.232 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52536,"remote_host":"[local]","session_id":"6ab89d00.cd38","line_num":1,"ps":"COPY","session_start":"2026-09-27 04:35:12 UTC","vxid":"95/557","txid":0,"error_severity":"ERROR","state_code":"23514","message":"new row for relation \"c\" violates check constraint \"c_amount_check\"","detail":"Failing row contains (11, 1, bad, -7).","context":"COPY c, line 2: \"11\t1\tbad\t-7\"","statement":"COPY c FROM stdin","application_name":"psql","backend_type":"client backend","query_id":4886255104157021466}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.299 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52543,"remote_host":"[local]","session_id":"6ab89d00.cd3f","line_num":1,"ps":"BIND","session_start":"2026-09-27 04:35:12 UTC","vxid":"10/531","txid":0,"error_severity":"ERROR","state_code":"22P02","message":"invalid input syntax for type integer: \"bind secret\"","context":"unnamed portal parameter $1 = 'bind secret'","statement":"SELECT $1::int ","application_name":"psql","backend_type":"client backend","query_id":7559284449764764609}` + "\n" +
		`{"timestamp":"2026-09-27 04:35:12.300 UTC","user":"postgres","dbname":"yc_redact_measure","pid":52543,"remote_host":"[local]","session_id":"6ab89d00.cd3f","line_num":2,"ps":"BIND","session_start":"2026-09-27 04:35:12 UTC","vxid":"10/532","txid":0,"error_severity":"ERROR","state_code":"22012","message":"division by zero","context":"unnamed portal with parameters: $1 = '5', $2 = 'param secret'","statement":"SELECT 1 / ($1::int - 5), $2::text ","application_name":"psql","backend_type":"client backend","query_id":6336613098166984710}` + "\n"
)

// errorStream is the measured file in each format: what the errors tail takes, and all of it.
type errorStream struct {
	format logFormat
	log    string
	taken  string
}

func errorStreams() []errorStream {
	return []errorStream{
		{
			format: logFormatStderr,
			log: measuredUniqueViolation + measuredWarning + measuredTimeoutBeside + measuredMissingDatabase +
				measuredUserCancel + measuredNowait + measuredReload,
			taken: measuredUniqueViolation + measuredMissingDatabase + measuredUserCancel + measuredNowait,
		},
		{
			format: logFormatCSV,
			log: measuredUniqueViolationCSV + measuredWarningCSV + measuredTimeoutBesideCSV + measuredMissingDatabaseCSV +
				measuredUserCancelCSV + measuredNowaitCSV + measuredReloadCSV,
			taken: measuredUniqueViolationCSV + measuredMissingDatabaseCSV + measuredUserCancelCSV + measuredNowaitCSV,
		},
		{
			format: logFormatJSON,
			log: measuredUniqueViolationJSON + measuredWarningJSON + measuredTimeoutBesideJSON + measuredMissingDatabaseJSON +
				measuredUserCancelJSON + measuredNowaitJSON + measuredReloadJSON,
			taken: measuredUniqueViolationJSON + measuredMissingDatabaseJSON + measuredUserCancelJSON + measuredNowaitJSON,
		},
	}
}

// ended follows a stderr entry with one that proves where it ends: until then the tail holds
// the entry back, since another continuation line could still arrive.
func ended(format logFormat, log string) string {
	if format == logFormatStderr {
		return log + unrelatedTraffic
	}

	return log
}

func TestErrorMatchIsTheThreeLevelsLessTheOtherTwoTails(t *testing.T) {
	assert.Equal(t, []string{"ERROR", "FATAL", "PANIC"}, errorMatch.severity)
	require.Len(t, errorMatch.exclude, 2)
	assert.Equal(t, deadlockMatch, errorMatch.exclude[0])
	assert.Equal(t, timeoutMatch, errorMatch.exclude[1])
}

func TestErrorMatchTakesEveryErrorAndFatalEntryVerbatim(t *testing.T) {
	for _, stream := range errorStreams() {
		t.Run(string(stream.format), func(t *testing.T) {
			body, matched := matchBody(stream.format, errorMatch, stream.log)

			assert.Equal(t, 4, matched,
				"the unique violation, the missing database, the client's cancel and the NOWAIT "+
					"refusal: a cancel and a NOWAIT share their codes with the timeouts, "+
					"which is why the timeouts tail pairs those codes with a message")
			assert.Equal(t, stream.taken, body,
				"in the file's own format and order, the statement timeout left to its own tail "+
					"and the WARNING and LOG lines to nobody")
		})
	}
}

func TestErrorMatchCopiesTheWholeEvent(t *testing.T) {
	body, matched := matchBody(logFormatStderr, errorMatch, measuredUniqueViolation+measuredReload)

	require.Equal(t, 1, matched)
	assert.Equal(t, measuredUniqueViolation, body)
	assert.Contains(t, body, "DETAIL:  Key (id)=(4021) already exists.")
	assert.Contains(t, body, "STATEMENT:  INSERT INTO yc_errs_orders",
		"the lines that follow the ERROR belong to it, as they do in pg_deadlocks.txt")
}

func TestErrorMatchLeavesDeadlocksAndTimeoutsToTheirOwnTails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format logFormat
		log    string
	}{
		{name: "stderr deadlock", format: logFormatStderr, log: measuredDeadlock},
		{name: "stderr statement timeout", format: logFormatStderr, log: measuredStatementTimeout},
		{name: "stderr lock timeout", format: logFormatStderr, log: measuredLockTimeout},
		{name: "stderr idle-in-transaction timeout", format: logFormatStderr, log: measuredIdleTimeout},
		{name: "csvlog deadlock", format: logFormatCSV, log: measuredDeadlockCSV},
		{
			name: "csvlog statement timeout", format: logFormatCSV,
			log: timeoutCSV("ERROR", "57014", "canceling statement due to statement timeout"),
		},
		{
			name: "csvlog lock timeout", format: logFormatCSV,
			log: timeoutCSV("ERROR", "55P03", "canceling statement due to lock timeout"),
		},
		{
			name: "csvlog idle-in-transaction timeout", format: logFormatCSV,
			log: timeoutCSV("FATAL", "25P03", "terminating connection due to idle-in-transaction timeout"),
		},
		{name: "jsonlog deadlock", format: logFormatJSON, log: measuredDeadlockJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := ended(tc.format, tc.log)

			_, own := matchBody(tc.format, deadlockMatch, log)
			_, timeouts := matchBody(tc.format, timeoutMatch, log)
			require.Equal(t, 1, own+timeouts, "the entry is its own tail's")

			_, matched := matchBody(tc.format, errorMatch, log)
			assert.Zero(t, matched, "so it is not written a second time")
		})
	}
}

// Every entry at ERROR, FATAL or PANIC is copied by exactly one of the three tails, and
// nothing below ERROR by the errors tail: none is lost between them, none written twice.
func TestEveryErrorEntryIsCopiedByExactlyOneTail(t *testing.T) {
	type entry struct {
		format  logFormat
		log     string
		isError bool
	}

	entries := []entry{
		{logFormatStderr, measuredUniqueViolation, true},
		{logFormatStderr, measuredWarning, false},
		{logFormatStderr, measuredTimeoutBeside, true},
		{logFormatStderr, measuredMissingDatabase, true},
		{logFormatStderr, measuredUserCancel, true},
		{logFormatStderr, measuredNowait, true},
		{logFormatStderr, measuredReload, false},
		{logFormatStderr, measuredDeadlock, true},
		{logFormatStderr, measuredLockTimeout, true},
		{logFormatStderr, measuredIdleTimeout, true},
		{logFormatStderr, unrelatedTraffic, false},
		{logFormatCSV, measuredUniqueViolationCSV, true},
		{logFormatCSV, measuredWarningCSV, false},
		{logFormatCSV, measuredTimeoutBesideCSV, true},
		{logFormatCSV, measuredMissingDatabaseCSV, true},
		{logFormatCSV, measuredUserCancelCSV, true},
		{logFormatCSV, measuredNowaitCSV, true},
		{logFormatCSV, measuredReloadCSV, false},
		{logFormatCSV, measuredDeadlockCSV, true},
		{logFormatCSV, unrelatedCSV, false},
		{logFormatJSON, measuredUniqueViolationJSON, true},
		{logFormatJSON, measuredWarningJSON, false},
		{logFormatJSON, measuredTimeoutBesideJSON, true},
		{logFormatJSON, measuredMissingDatabaseJSON, true},
		{logFormatJSON, measuredUserCancelJSON, true},
		{logFormatJSON, measuredNowaitJSON, true},
		{logFormatJSON, measuredReloadJSON, false},
		{logFormatJSON, measuredDeadlockJSON, true},
		{logFormatJSON, unrelatedJSON, false},
	}

	for i, e := range entries {
		first, _, _ := strings.Cut(e.log, "\n")

		var copiedBy []string

		for name, m := range map[string]eventMatch{
			"pg_deadlocks": deadlockMatch, "pg_timeouts": timeoutMatch, "errors": errorMatch,
		} {
			if _, matched := matchBody(e.format, m, ended(e.format, e.log)); matched > 0 {
				copiedBy = append(copiedBy, name)
			}
		}

		if e.isError {
			assert.Len(t, copiedBy, 1, "entry %d (%s) %s", i, e.format, first)
		} else {
			assert.Empty(t, copiedBy, "entry %d (%s) %s", i, e.format, first)
		}
	}
}

// Constructed, not measured: a PANIC takes the server down, and the matrix servers are shared.
func TestErrorMatchTakesAPanic(t *testing.T) {
	panicLine := "2026-09-27 01:51:00.000 UTC [31] PANIC:  could not write to file \"pg_wal/xlogtemp.31\": No space left on device\n"

	body, matched := matchBody(logFormatStderr, errorMatch, panicLine+measuredReload)
	require.Equal(t, 1, matched)
	assert.Equal(t, panicLine, body)

	record := strings.Replace(measuredMissingDatabaseCSV, ",FATAL,3D000,", ",PANIC,53100,", 1)
	_, matched = matchBody(logFormatCSV, errorMatch, record)
	assert.Equal(t, 1, matched)

	line := strings.Replace(measuredMissingDatabaseJSON, `"error_severity":"FATAL"`, `"error_severity":"PANIC"`, 1)
	_, matched = matchBody(logFormatJSON, errorMatch, line)
	assert.Equal(t, 1, matched)
}

// Constructed: the German catalogue's word for ERROR. A translated level is not taken, as no
// tail takes a translated keyword; matched=0 there under-reports rather than mis-bounds.
func TestErrorMatchReadsTheEnglishLevelsOnly(t *testing.T) {
	translated := strings.Replace(measuredUniqueViolation, "] ERROR:  ", "] FEHLER:  ", 1)

	_, matched := matchBody(logFormatStderr, errorMatch, translated)
	assert.Zero(t, matched)

	_, matched = matchBody(logFormatCSV, errorMatch,
		strings.Replace(measuredUniqueViolationCSV, ",ERROR,23505,", ",FEHLER,23505,", 1))
	assert.Zero(t, matched)
}

func TestStderrSeverityNamesTheLevel(t *testing.T) {
	for _, tc := range []struct {
		line, severity string
	}{
		{measuredMissingDatabase, "FATAL"},
		{measuredUserCancel, "ERROR"},
		{measuredWarning, "WARNING"},
		{measuredReload, "LOG"},
		{"2026-09-27 01:50:00.075 UTC [12463] STATEMENT:  SELECT pg_sleep(5)", ""},
	} {
		first, _, _ := strings.Cut(tc.line, "\n")

		at, severity, _ := stderrSeverity(first)

		assert.Equal(t, tc.severity, severity, first)
		assert.Equal(t, tc.severity == "", at < 0, first)
	}
}

func TestErrorsArtifact(t *testing.T) {
	artifact := NewErrors().Artifact()

	assert.Equal(t, "pg_errors", artifact.Name)
	assert.Equal(t, "pg_errors.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope)
	assert.Equal(t, formatText, artifact.Format, "the body is the server's log bytes")
	assert.Equal(t, Every(DefaultLogTailInterval), artifact.Schedule, "the other tails' 10s poll")

	var _ Collector = NewErrors()
	var _ Closing = NewErrors()
}

// The measured stream across a 30s window: the WARNING ends the unique violation in
// the first write, and the NOWAIT refusal ends the client's cancel held since the second.
var errorsGoldenWrites = []string{
	measuredUniqueViolation + measuredWarning,
	measuredTimeoutBeside + measuredMissingDatabase + measuredUserCancel,
	measuredNowait + measuredReload,
}

func TestErrorsGoldenFull(t *testing.T) {
	results := runLogGoldenWindow(t, NewErrors(), logFormatStderr,
		priorTraffic, errorsGoldenWrites, 30*time.Second, logGoldenClock(t, 3))

	require.Equal(t, StatusComplete, results[0].Status)

	artifact := artifactText(t, results[0])

	assert.Contains(t, artifact, "matched_by=severity", "by level, where the other tails match by code or message")
	assert.NotContains(t, artifact, "statement timeout", "pg_timeouts.txt's")
	assert.NotContains(t, artifact, "WARNING:", "below the three levels")

	assert.Equal(t, bloatGolden(t, "pg_errors_full.txt"), artifact)
}

func TestErrorsGoldenUnreadable(t *testing.T) {
	results := runRemoteGoldenWindow(t, NewErrors(), 30*time.Second, logGoldenClock(t, 3))

	require.Equal(t, StatusComplete, results[0].Status)

	artifact := artifactText(t, results[0])

	assert.NotContains(t, artifact, "matched=", "no count beside a log that was never read")

	assert.Equal(t, bloatGolden(t, "pg_errors_unreadable.txt"), artifact)
}

func TestErrorsRedactionReplacesEveryValueTheMeasuredErrorsQuote(t *testing.T) {
	written := strings.NewReplacer(
		"Key (id)=(1) already exists.", "Key (id)=(<redacted>) already exists.",
		"STATEMENT:  INSERT INTO p VALUES (1);", "STATEMENT:  <redacted>",
		"Key (pid)=(99) is not present", "Key (pid)=(<redacted>) is not present",
		"STATEMENT:  INSERT INTO c VALUES (1, 99, 'fk secret', 5);", "STATEMENT:  <redacted>",
		"Failing row contains (2, 1, check secret, -5).", "Failing row contains (<redacted>).",
		"STATEMENT:  INSERT INTO c VALUES (2, 1, 'check secret', -5);", "STATEMENT:  <redacted>",
		"Failing row contains (3, 1, null, 5).", "Failing row contains (<redacted>).",
		"STATEMENT:  INSERT INTO c VALUES (3, 1, NULL, 5);", "STATEMENT:  <redacted>",
		`integer: "abc secret" at character 8`, `integer: "<redacted>" at character 8`,
		"STATEMENT:  SELECT 'abc secret'::int;", "STATEMENT:  <redacted>",
		`Token "secret" is invalid.`, `Token "<redacted>" is invalid.`,
		`JSON data, line 1: {"card": 4111 secret...`, `JSON data, line 1: <redacted>`,
		`STATEMENT:  SELECT '{"card": 4111 secret}'::json;`, "STATEMENT:  <redacted>",
		`integer: "multi line secret" at character 13`, `integer: "<redacted>" at character 13`,
		"STATEMENT:  SELECT 1,\n\t  'multi line secret'::int;", "STATEMENT:  <redacted>",
		"STATEMENT:  DO $$ BEGIN RAISE EXCEPTION 'customer % over limit', 12345 USING DETAIL = 'balance 99.50', HINT = 'call 555-0100'; END $$;",
		"STATEMENT:  <redacted>",
		"QUERY:  SELEC 'exec secret'", "QUERY:  <redacted>",
		"STATEMENT:  DO $$ BEGIN EXECUTE 'SELEC ''exec secret'''; END $$;", "STATEMENT:  <redacted>",
		`SQL statement "INSERT INTO p VALUES (1)"`, `SQL statement "<redacted>"`,
		"STATEMENT:  DO $$ BEGIN INSERT INTO p VALUES (1); END $$;", "STATEMENT:  <redacted>",
		`integer: "x secret"`, `integer: "<redacted>"`,
		`column id: "x secret"`, `column id: "<redacted>"`,
		"STATEMENT:  COPY c FROM stdin", "STATEMENT:  <redacted>",
		"Failing row contains (11, 1, bad, -7).", "Failing row contains (<redacted>).",
		"COPY c, line 2: \"11\t1\tbad\t-7\"", `COPY c, line 2: "<redacted>"`,
		`integer: "bind secret"`, `integer: "<redacted>"`,
		"$1 = 'bind secret'", "$1 = '<redacted>'",
		"STATEMENT:  SELECT $1::int \n", "STATEMENT:  <redacted>\n",
		"$1 = '5', $2 = 'param secret'", "$1 = '<redacted>', $2 = '<redacted>'",
		"STATEMENT:  SELECT 1 / ($1::int - 5), $2::text \n", "STATEMENT:  <redacted>\n",
	)

	for _, stream := range []struct {
		format logFormat
		log    string
	}{
		{logFormatStderr, measuredValueErrors},
		{logFormatCSV, measuredValueErrorsCSV},
		{logFormatJSON, measuredValueErrorsJSON},
	} {
		t.Run(string(stream.format), func(t *testing.T) {
			body, matched, redacted := matchWritten(stream.format, errorMatch, NewErrors().tail.redaction,
				ended(stream.format, stream.log))

			require.Equal(t, 15, matched)

			if stream.format == logFormatStderr {
				assert.Equal(t, written.Replace(measuredValueErrors), body,
					"the messages, codes, names, positions and frames stay; the values go")
			}

			assert.NotContains(t, body, "secret", "no value, statement, internal query or bound parameter survives")
			assert.Equal(t, 35, redacted, "one for each replacement, the same in every format")

			for _, kept := range []string{
				"duplicate key value violates unique constraint", "p_pkey", "c_pid_fkey", "c_amount_check",
				"null value in column", "syntax error at or near", "SELEC",
				"PL/pgSQL function inline_code_block line 1 at SQL statement",
				"Key (id)=(<redacted>) already exists.", "Key (pid)=(<redacted>) is not present in table",
				"Failing row contains (<redacted>).", "unnamed portal with parameters: $1 = '<redacted>', $2 = '<redacted>'",
			} {
				assert.Contains(t, body, kept)
			}

			assert.Contains(t, body, "customer 12345 over limit",
				"an application's own RAISE text is in no shape the agent knows, and stays")
		})
	}
}

func TestErrorsRedactionOfTheStreamsTheGoldenReads(t *testing.T) {
	for _, stream := range errorStreams() {
		t.Run(string(stream.format), func(t *testing.T) {
			body, matched, redacted := matchWritten(stream.format, errorMatch, NewErrors().tail.redaction,
				ended(stream.format, stream.log))

			require.Equal(t, 4, matched)
			assert.Equal(t, 4, redacted, "the key's value and three statements: the FATAL at connection has none")
			assert.NotContains(t, body, "4021")
			assert.NotContains(t, body, "pending")
			assert.Contains(t, body, "yc_no_such_database", "a database's name is a name")
		})
	}
}

func TestRedactMessage(t *testing.T) {
	for _, tt := range []struct {
		message string
		want    string
	}{
		// Values.
		{`invalid input syntax for type integer: "abc secret" at character 8`, `invalid input syntax for type integer: "<redacted>" at character 8`},
		{`22P02: invalid input syntax for type uuid: "abc"`, `22P02: invalid input syntax for type uuid: "<redacted>"`},
		{`invalid input value for enum mood: "sad"`, `invalid input value for enum mood: "<redacted>"`},
		{`value "99999999999" is out of range for type integer`, `value "<redacted>" is out of range for type integer`},
		{`"1e400" is out of range for type double precision`, `"<redacted>" is out of range for type double precision`},
		{`date/time field value out of range: "2026-13-45"`, `date/time field value out of range: "<redacted>"`},
		{`invalid value "xx" for "MM"`, `invalid value "<redacted>" for "MM"`},
		{`malformed array literal: "{1,2"`, `malformed array literal: "<redacted>"`},
		{`invalid byte sequence for encoding "UTF8": 0xff 0xfe`, `invalid byte sequence for encoding "UTF8": <redacted>`},
		{`unterminated quoted string at or near "'abc" at character 8`, `unterminated quoted string at or near "<redacted>" at character 8`},
		{`syntax error at or near "'secret'" at character 15`, `syntax error at or near "<redacted>" at character 15`},
		{`syntax error at or near "42"`, `syntax error at or near "<redacted>"`},
		{`invalid value for parameter "work_mem": "lots"`, `invalid value for parameter "work_mem": "<redacted>"`},

		// Names, and messages that quote nothing: the shapes that must not match.
		{`syntax error at or near "SELEC" at character 1`, `syntax error at or near "SELEC" at character 1`},
		{`duplicate key value violates unique constraint "p_pkey"`, `duplicate key value violates unique constraint "p_pkey"`},
		{`relation "orders" does not exist`, `relation "orders" does not exist`},
		{`null value in column "note" of relation "c" violates not-null constraint`, `null value in column "note" of relation "c" violates not-null constraint`},
		{`invalid input syntax for type json`, `invalid input syntax for type json`},
		{`division by zero`, `division by zero`},
	} {
		t.Run(tt.message, func(t *testing.T) {
			got, redacted := redactMessage(tt.message)

			assert.Equal(t, tt.want, got)
			assert.Equal(t, strings.Count(tt.want, redactedValue), redacted)
		})
	}
}

func TestRedactDetail(t *testing.T) {
	for _, tt := range []struct {
		name     string
		sqlstate string
		detail   string
		want     string
	}{
		{name: "a unique key", detail: "Key (id)=(1) already exists.", want: "Key (id)=(<redacted>) already exists."},
		{name: "a missing referenced key", detail: `Key (pid)=(99) is not present in table "p".`, want: `Key (pid)=(<redacted>) is not present in table "p".`},
		{name: "a key still referenced", detail: `Key (id)=(5) is still referenced from table "c".`, want: `Key (id)=(<redacted>) is still referenced from table "c".`},
		{name: "an exclusion conflict", detail: "Key (a)=(1) conflicts with existing key (a)=(2).", want: "Key (a)=(<redacted>) conflicts with existing key (a)=(<redacted>)."},
		{name: "an expression index", detail: "Key (lower(email::text))=(a@example.com) already exists.", want: "Key (lower(email::text))=(<redacted>) already exists."},
		{name: "a value holding a parenthesis", detail: "Key (note)=(a) b) already exists.", want: "Key (note)=(<redacted>) already exists."},
		{name: "a failing row", detail: "Failing row contains (2, 1, check secret, -5).", want: "Failing row contains (<redacted>)."},
		{name: "a partition key", detail: "Partition key of the failing row contains (region) = (eu).", want: "Partition key of the failing row contains (region) = (<redacted>)."},
		{name: "a json token", detail: `Token "secret" is invalid.`, want: `Token "<redacted>" is invalid.`},
		{name: "a json expectation", detail: `Expected ":", but found "x".`, want: `Expected ":", but found "<redacted>".`},
		{
			name:   "a translated key, by its shape",
			detail: "Schlüssel »(id)=(4021)« existiert bereits.",
			want:   "Schlüssel »(id)=(<redacted>)« existiert bereits.",
		},
		{
			name: "a translated failing row, by its code", sqlstate: "23514",
			detail: "Fehlgeschlagene Zeile enthält (2, 1, x, -5).",
			want:   "<redacted>",
		},
		{
			name:   "the same without a code: stderr, where only English is matched",
			detail: "Fehlgeschlagene Zeile enthält (2, 1, x, -5).",
			want:   "Fehlgeschlagene Zeile enthält (2, 1, x, -5).",
		},
		{name: "a server's own explanation", sqlstate: "55006", detail: "There are 2 other sessions using the database.", want: "There are 2 other sessions using the database."},
		{name: "an application's own RAISE detail", sqlstate: "P0001", detail: "balance 99.50", want: "balance 99.50"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, redacted := redactDetail(tt.sqlstate, tt.detail)

			assert.Equal(t, tt.want, got)
			assert.Equal(t, strings.Count(tt.want, redactedValue), redacted)
		})
	}
}

func TestErrorsAndDeadlocksShareTheirRules(t *testing.T) {
	statement := "ERROR:  boom\nQUERY:  SELECT 'a'\nCONTEXT:  SQL statement \"SELECT 'a'\"\nSTATEMENT:  SELECT 'a'\n"

	errorsGot, errorsRedacted := NewErrors().tail.redaction.event([]byte(statement), logFormatStderr)
	deadlocksGot, deadlocksRedacted := NewDeadlocks().tail.redaction.event([]byte(statement), logFormatStderr)

	assert.Equal(t, string(errorsGot), string(deadlocksGot), "QUERY, CONTEXT and STATEMENT alike in both files")
	assert.Equal(t, 3, errorsRedacted)
	assert.Equal(t, errorsRedacted, deadlocksRedacted)
}
