const assert = require("assert");
const V = require("../extension/lib.js");
const from = "2026-11-11";
// AIS days.json
assert.deepStrictEqual(V.extractDates([{date:"2027-01-15",business_day:true},{date:"2026-12-03",business_day:true},{date:"2026-12-04",business_day:false}], from), ["2026-12-03","2027-01-15"]);
// CGI style
assert.deepStrictEqual(V.extractDates({ScheduleDays:[{ID:"x",Date:"2027-02-01T00:00:00"},{ID:"y",Date:"2026-11-20T00:00:00"}]}, from), ["2026-11-20","2027-02-01"]);
// past dates, birth dates and unavailable days ignored
assert.deepStrictEqual(V.extractDates({DateOfBirth:"2027-05-05",days:[{date:"2026-01-01"},{Date:"2027-03-03",IsAvailable:false},{Date:"2027-03-04"}]}, from), ["2027-03-04"]);
assert.deepStrictEqual(V.extractDates(["2027-01-02","2027-01-01","2027-01-01"], from), ["2027-01-01","2027-01-02"]);
assert.deepStrictEqual(V.extractDates(null, from), []);
const u = V.parseAisUrl("https://ais.usvisa-info.com/es-mx/niv/schedule/51234567/appointment");
assert.deepStrictEqual(u, {locale:"es-mx", cc:"MX", scheduleId:"51234567"});
assert.strictEqual(V.aisDaysUrl(u, 65), "https://ais.usvisa-info.com/es-mx/niv/schedule/51234567/appointment/days/65.json?appointments[expedite]=false");
assert.strictEqual(V.parseAisUrl("https://ais.usvisa-info.com/en-ca/niv/users/sign_in"), null);
assert(V.looksLikeSlotUrl("/en-US/custom-actions/?route=/api/v1/schedule-group/get-family-consular-schedule-days&x=1"));
assert(!V.looksLikeSlotUrl("/en-US/static/app.css"));
// US format (what usvisascheduling.com displays), slashes, and .NET dates
assert.deepStrictEqual(V.extractDates({ScheduleDays:[{Date:"12/18/2026"},{Date:"1/5/2027"}]}, from), ["2026-12-18","2027-01-05"]);
assert.deepStrictEqual(V.extractDates([{SlotDate:"2026/12/20"}], from), ["2026-12-20"]);
assert.deepStrictEqual(V.extractDates([{Date:"/Date(1797552000000)/"}], from), [new Date(1797552000000).toISOString().slice(0,10)]);
assert.deepStrictEqual(V.extractDates([{Date:"13/40/2026"}], from), []);
console.log("lib tests: all passed");
