package ui

// EventsSchema is the version 1 NDJSON event contract. Individual events may
// add fields; consumers must ignore fields they do not understand.
func EventsSchema() []byte {
	return []byte(`{
 "$schema":"https://json-schema.org/draft/2020-12/schema",
 "$id":"https://yohobuild.com/schema/events-v1.schema.json",
 "title":"Yoho NDJSON event v1",
 "type":"object",
 "required":["schema_version","event","time"],
 "properties":{
  "schema_version":{"const":1},
  "event":{"type":"string","minLength":1},
  "time":{"type":"string","format":"date-time"},
  "operation_id":{"type":"string","pattern":"^[A-Za-z0-9_-]{1,128}$"},
  "outcome":{"enum":["success","partial","failed","unknown"]},
  "changed":{"type":"array","items":{"type":"string"}},
  "unchanged":{"type":"array","items":{"type":"string"}},
  "duration_ms":{"type":"integer","minimum":0}
 },
 "allOf":[
  {"if":{"properties":{"event":{"const":"finished"}},"required":["event"]},"then":{"required":["outcome","duration_ms"]}},
  {"if":{"properties":{"outcome":{"const":"partial"}},"required":["outcome"]},"then":{"required":["changed","unchanged"]}},
  {"if":{"properties":{"event":{"const":"operation_started"}},"required":["event"]},"then":{"required":["operation_id"]}}
 ],
 "additionalProperties":true
}`)
}
