// Run with: node --test internal/observability/dashboard/app.test.cjs
const {test}=require("node:test");
const assert=require("node:assert/strict");
const fs=require("node:fs");
const path=require("node:path");
const vm=require("node:vm");
const source=fs.readFileSync(path.join(__dirname,"app.js"),"utf8");

function harness(){
  let now=1700000000000, sample={at:now,values:{}}, history=[];
  const elements=new Map(),timers=[];
  function element(){return {textContent:"",className:"",children:[],events:{},width:800,height:220,append(...items){this.children.push(...items);},replaceChildren(...items){this.children=items;},addEventListener(name,fn){this.events[name]=fn;},getContext(){return new Proxy({}, {get:(_,key)=>key==="canvas"?this:()=>{},set:()=>true});}};}
  const document={getElementById(id){if(!elements.has(id))elements.set(id,element());return elements.get(id);},createElement:element,createDocumentFragment:element,documentElement:{}};
  // Card IDs become addressable when the browser appends them.
  const create=document.createElement;document.createElement=()=>{const item=create();Object.defineProperty(item,"id",{set(id){elements.set(id,item);}});return item;};
  const context={document,console,Map,Object,Number,JSON,Math,AbortController,Date:class extends Date{static now(){return now;}},getComputedStyle:()=>({getPropertyValue:()=>"#8899aa"}),setTimeout(fn,ms){if(ms===5000)timers.push(fn);return 1;},clearTimeout(){},fetch:async url=>({ok:true,json:async()=>url.startsWith("/api/v1/current")?sample:{available:true,samples:history}})};
  vm.runInNewContext(source,context);
  return {elements,async flush(){await new Promise(setImmediate);},async next(values,advance=5000){now+=advance;sample={at:now,values};const next=timers.shift();assert.ok(next,"refresh should schedule only after preceding refresh finishes");await next();await new Promise(setImmediate);},setHistory(rows){history=rows;},current(values){sample={at:now,values};},text(id){return elements.get(id)?.textContent;}};
}

const counters=(cpu,success,error,buckets=[0,0,0])=>({
  gortex_process_cpu_seconds_total:cpu,
  'gortex_mcp_requests_total{outcome="success"}':success,
  'gortex_mcp_requests_total{outcome="error"}':error,
  'gortex_mcp_request_duration_seconds_bucket{le="0.1"}':buckets[0],
  'gortex_mcp_request_duration_seconds_bucket{le="0.25"}':buckets[1],
  'gortex_mcp_request_duration_seconds_bucket{le="+Inf"}':buckets[2]
});

test("missing values remain unknown, CPU/rates use deltas and histogram bounds",async()=>{
  const ui=harness();ui.current(counters(10,100,4));await ui.flush();
  assert.equal(ui.text("value-cpu"),"Unknown");
  assert.equal(ui.text("value-rss"),"Unknown");
  assert.equal(ui.text("value-latency"),"Unknown");
  await ui.next(counters(11,148,6,[45,49,50]));
  assert.equal(ui.text("value-cpu"),"20%");
  assert.equal(ui.text("value-rate"),"10 /s");
  assert.equal(ui.text("value-errors"),"0.4 /s");
  assert.equal(ui.text("value-latency"),"193.8 ms");
  await ui.next(counters(1,1,0,[0,0,0]));
  assert.equal(ui.text("value-cpu"),"Unknown");
  assert.equal(ui.text("value-rate"),"Unknown");
  assert.equal(ui.text("value-latency"),"Unknown");
  await ui.next(counters(2,2,0,[1,1,1]),60000);
  assert.equal(ui.text("value-cpu"),"Unknown","long gaps must not turn into low averaged usage");
  assert.equal(ui.text("value-rate"),"Unknown");
});

test("labels are text, metadata is advisory and pausing cannot stop collection",async()=>{
  const ui=harness();const name='<img src=x onerror="alert(1)">';
  ui.current({gortex_daemon_ready:1,gortex_daemon_enrichment_complete:0,[`gortex_repo_nodes{repo=${JSON.stringify(name)}}`]:42});await ui.flush();
  assert.equal(ui.text("ready"),"References ready");
  assert.equal(ui.text("enrichment"),"Enrichment running");
  const fragment=ui.elements.get("repo-rows").children[0];
  assert.equal(fragment.children[0].children[0].textContent,name);
  assert.equal(fragment.children[0].children[4].textContent,"Unknown / metadata only");
  ui.elements.get("pause").events.click();
  assert.equal(ui.text("connection"),"View paused · collection continues");
  assert.equal(ui.text("pause"),"Resume view");
});

test("uptime detects a restart even when replacement counters exceed the prior process",async()=>{
  const ui=harness();ui.current({...counters(1,1,0),gortex_daemon_uptime_seconds:500});await ui.flush();
  await ui.next({...counters(20,100,1,[45,49,50]),gortex_daemon_uptime_seconds:3});
  assert.equal(ui.text("value-cpu"),"Unknown");
  assert.equal(ui.text("value-rate"),"Unknown");
  assert.equal(ui.text("value-latency"),"Unknown");
});

test("an infinite histogram tail never becomes an invented finite p95",async()=>{
  const ui=harness();ui.current(counters(1,0,0));await ui.flush();
  await ui.next(counters(2,100,0,[1,2,100]));
  assert.equal(ui.text("value-latency"),"Unknown");
});
