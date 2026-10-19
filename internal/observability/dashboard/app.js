"use strict";
(() => {
  const $ = id => document.getElementById(id);
  const state = {paused:false, current:null, previous:null, history:[], historyAt:0, historyAvailable:false, window:3600};
  const defs = [
    ["cpu", "Daemon CPU", "% of one CPU core; counter delta"],
    ["rss", "Resident memory", "Daemon RSS; shared pages included"],
    ["heap", "Live Go heap", "Allocated objects, distinct from RSS"],
    ["cache", "Trigram cache", "Estimated live cache heap"],
    ["goroutines", "Goroutines", "Current runtime count"],
    ["rate", "MCP throughput", "Completed requests per second"],
    ["errors", "MCP non-success", "Errors, cancellation, deadline and busy"],
    ["latency", "MCP p95 latency", "Histogram estimate over latest interval"]
  ];
  for (const [id,title,note] of defs) {
    const card=document.createElement("article"); card.className="card";
    const heading=document.createElement("h2"); heading.textContent=title;
    const value=document.createElement("strong"); value.id="value-"+id; value.textContent="Unknown";
    const description=document.createElement("p"); description.textContent=note;
    card.append(heading,value,description); $("cards").append(card);
  }
  const known = n => typeof n === "number" && Number.isFinite(n);
  const fmt = (n,digits=1) => known(n) ? n.toLocaleString(undefined,{maximumFractionDigits:digits}) : "Unknown";
  const bytes = n => known(n) ? (n>=1073741824 ? fmt(n/1073741824,2)+" GiB" : fmt(n/1048576)+" MiB") : "Unknown";
  function entries(sample,name) {
    const result=[];
    for(const [key,value] of Object.entries(sample?.values || {})) {
      if(key!==name && !key.startsWith(name+"{")) continue;
      if(!known(value)) continue;
      const labels=Object.create(null);
      const raw=key.slice(name.length);
      for(const match of raw.matchAll(/([a-zA-Z_][a-zA-Z0-9_]*)="((?:\\.|[^"\\])*)"/g)) {
        try { labels[match[1]]=JSON.parse('"'+match[2]+'"'); } catch { /* malformed label is unavailable */ }
      }
      result.push({value,labels,key});
    }
    return result;
  }
  function scalar(sample,name) {const n=sample?.values?.[name];return known(n)?n:null;}
  function sum(sample,name,predicate=()=>true) {const values=entries(sample,name).filter(x=>predicate(x.labels));return values.length?values.reduce((n,x)=>n+x.value,0):null;}
  function restarted(previous,sample) {
    const before=scalar(previous,"gortex_daemon_uptime_seconds"), after=scalar(sample,"gortex_daemon_uptime_seconds");
    return known(before)&&known(after)&&after<before;
  }
  function delta(previous,sample,name,predicate,maxGap=45) {
    if(!previous || !sample || restarted(previous,sample)) return null;
    const seconds=(sample.at-previous.at)/1000;
    if(seconds<=0 || seconds>maxGap) return null;
    const a=sum(previous,name,predicate), b=sum(sample,name,predicate);
    return known(a)&&known(b)&&b>=a?(b-a)/seconds:null;
  }
  function histogram(sample) {
    const buckets=new Map();
    for(const item of entries(sample,"gortex_mcp_request_duration_seconds_bucket")) {
      const bound=item.labels.le==="+Inf"?Infinity:Number(item.labels.le);
      if(!Number.isNaN(bound)) buckets.set(bound,(buckets.get(bound)||0)+item.value);
    }
    return buckets;
  }
  function quantile(previous,sample,maxGap=45) {
    if(!previous || !sample || restarted(previous,sample) || sample.at<=previous.at || (sample.at-previous.at)/1000>maxGap) return null;
    const a=histogram(previous), b=histogram(sample), rows=[];
    if(!a.has(Infinity)||!b.has(Infinity)||a.size!==b.size) return null;
    for(const [bound,count] of b) {if(!a.has(bound)||count<a.get(bound)) return null;rows.push([bound,count-a.get(bound)]);}
    rows.sort((x,y)=>x[0]-y[0]); const total=b.get(Infinity)-a.get(Infinity);
    if(total<=0) return null;
    const target=total*.95; let lowerBound=0,lowerCount=0;
    for(const [bound,count] of rows) {
      if(count<lowerCount) return null;
      if(count>=target) {
        if(!Number.isFinite(bound) || count===lowerCount) return null;
        return (lowerBound+(bound-lowerBound)*(target-lowerCount)/(count-lowerCount))*1000;
      }
      lowerBound=bound;lowerCount=count;
    }
    return null;
  }
  function status(id,text,kind="") {const element=$(id);element.textContent=text;element.className=kind;}
  function renderCurrent() {
    const sample=state.current, previous=state.previous;
    const fresh=sample && known(sample.at) && sample.at>0 && Date.now()-sample.at<45000;
    if(!fresh) {
      for(const [id] of defs) $("value-"+id).textContent="Unknown";
      status("ready","Readiness unknown");status("phase","Phase unknown");status("enrichment","Enrichment unknown");status("sessions","Sessions unknown");status("children","Child processes unknown");
      $("sample-time").textContent=sample?.at?"Last sample "+new Date(sample.at).toLocaleString()+" (stale)":"No current sample available";
      $("repo-rows").replaceChildren();$("repo-status").textContent="Repository snapshots unavailable.";
      return;
    }
    const ready=scalar(sample,"gortex_daemon_ready"), enriched=scalar(sample,"gortex_daemon_enrichment_complete");
    status("ready",ready===1?"References ready":ready===0?"Warming up":"Readiness unknown",ready===1?"good":"pending");
    status("enrichment",enriched===1?"Enrichment complete":enriched===0?"Enrichment running":"Enrichment unknown",enriched===1?"good":"pending");
    const phase=entries(sample,"gortex_daemon_phase").find(x=>x.value===1)?.labels.phase;
    status("phase",phase?"Phase: "+phase:"Phase unknown");
    const sessions=entries(sample,"gortex_daemon_sessions").map(x=>(x.labels.state||"unknown")+": "+fmt(x.value,0));
    status("sessions",sessions.length?sessions.join(" · "):"Sessions unknown");
    const children=scalar(sample,"gortex_child_processes"),childRSS=scalar(sample,"gortex_children_resident_memory_bytes"),childComplete=scalar(sample,"gortex_children_sample_complete");
    status("children",known(children)?"Children: "+fmt(children,0)+" · RSS "+bytes(childRSS)+(childComplete===1?"":" (partial snapshot)"):"Child processes unknown");
    const cpu=delta(previous,sample,"gortex_process_cpu_seconds_total");
    $("value-cpu").textContent=known(cpu)?fmt(cpu*100)+"%":"Unknown";
    $("value-rss").textContent=bytes(scalar(sample,"gortex_process_resident_memory_bytes"));
    $("value-heap").textContent=bytes(scalar(sample,"gortex_runtime_heap_alloc_bytes"));
    $("value-cache").textContent=bytes(sum(sample,"gortex_cache_bytes",x=>x.cache==="trigram"));
    $("value-goroutines").textContent=fmt(scalar(sample,"gortex_runtime_goroutines"),0);
    const rate=delta(previous,sample,"gortex_mcp_requests_total"), errors=delta(previous,sample,"gortex_mcp_requests_total",x=>x.outcome!=="success");
    $("value-rate").textContent=known(rate)?fmt(rate,2)+" /s":"Unknown";
    $("value-errors").textContent=known(errors)?fmt(errors,2)+" /s":"Unknown";
    const latency=quantile(previous,sample);$("value-latency").textContent=known(latency)?fmt(latency)+" ms":"Unknown";
    $("sample-time").textContent="Sample "+new Date(sample.at).toLocaleString();
    renderRepos(sample);
  }
  function renderRepos(sample) {
    const repos=new Map();
    for(const [name,field] of [["gortex_repo_files","files"],["gortex_repo_nodes","nodes"],["gortex_repo_edges","edges"],["gortex_repo_indexed","indexed"]]) {
      for(const item of entries(sample,name)) {const repo=item.labels.repo;if(!repo)continue;if(!repos.has(repo)&&repos.size>=512)continue;const row=repos.get(repo)||{};row[field]=item.value;repos.set(repo,row);}
    }
    const source=entries(sample,"gortex_daemon_aggregate_source").find(x=>x.value===1)?.labels.source;
    const snapshot=scalar(sample,"gortex_daemon_aggregate_snapshot_timestamp_seconds");
    $("repo-status").textContent=(source?"Source: "+source+". ":"")+(known(snapshot)?"Snapshot "+new Date(snapshot*1000).toLocaleString()+". ":"")+"Advisory counts; metadata does not prove query readiness. Up to 512 repositories shown.";
    const fragment=document.createDocumentFragment();
    for(const [name,row] of [...repos].sort((a,b)=>a[0].localeCompare(b[0]))) {
      const tr=document.createElement("tr");
      for(const value of [name,fmt(row.files,0),fmt(row.nodes,0),fmt(row.edges,0),row.indexed===1?"Loaded (snapshot)":row.indexed===0?"Not loaded (snapshot)":"Unknown / metadata only"]) {const td=document.createElement("td");td.textContent=value;tr.append(td);}fragment.append(tr);
    }
    $("repo-rows").replaceChildren(fragment);
  }
  function chart(id,values,color) {
    const canvas=$(id),context=canvas.getContext("2d");if(!context)return;
    const w=canvas.width,h=canvas.height,pad=36, bottom=h-25;context.clearRect(0,0,w,h);
    const style=getComputedStyle(document.documentElement),muted=style.getPropertyValue("--muted").trim(),line=style.getPropertyValue("--line").trim();
    const valid=values.filter(x=>known(x[1]));const maximum=Math.max(1,...valid.map(x=>x[1]));
    context.font="11px system-ui";context.textAlign="right";context.fillStyle=muted;context.strokeStyle=line;
    for(let i=0;i<3;i++){const y=12+(bottom-12)*i/2;context.beginPath();context.moveTo(pad,y);context.lineTo(w-8,y);context.stroke();context.fillText(fmt(maximum*(1-i/2)),pad-5,y+4);}
    const end=Date.now(),start=end-state.window*1000;let drawing=false;context.strokeStyle=color;context.lineWidth=2;context.beginPath();
    for(const [at,value] of values) {if(!known(value)||at<start||at>end){drawing=false;continue;}const x=pad+(w-pad-8)*(at-start)/(end-start),y=bottom-(bottom-12)*value/maximum;if(drawing)context.lineTo(x,y);else context.moveTo(x,y);drawing=true;}
    context.stroke();context.textAlign="left";context.fillStyle=muted;context.fillText(new Date(start).toLocaleTimeString([], {hour:"2-digit",minute:"2-digit"}),pad,h-5);context.textAlign="right";context.fillText("now",w-8,h-5);
    $(id+"-info").textContent=valid.length?fmt(valid.length,0)+" observations · gaps and counter resets are not interpolated":"No data for this window";
  }
  function renderHistory() {
    const rows=state.history, gap=Math.max(45,state.window/360*4), cpu=[],memory=[],requests=[],latency=[];
    for(let i=0;i<rows.length;i++){const sample=rows[i],previous=rows[i-1],cpuRate=delta(previous,sample,"gortex_process_cpu_seconds_total",undefined,gap);cpu.push([sample.at,known(cpuRate)?cpuRate*100:null]);memory.push([sample.at,known(scalar(sample,"gortex_process_resident_memory_bytes"))?scalar(sample,"gortex_process_resident_memory_bytes")/1048576:null]);requests.push([sample.at,delta(previous,sample,"gortex_mcp_requests_total",undefined,gap)]);latency.push([sample.at,quantile(previous,sample,gap)]);if(previous&&((sample.at-previous.at)/1000>gap||restarted(previous,sample))){memory.splice(memory.length-1,0,[sample.at-1,null]);}}
    chart("cpu-chart",cpu,"#4ecdb4");chart("memory-chart",memory,"#789fff");chart("requests-chart",requests,"#c095f6");chart("latency-chart",latency,"#e9b45e");
  }
  async function getJSON(path) {
    const controller=new AbortController(),timer=setTimeout(()=>controller.abort(),4500);
    try {const response=await fetch(path,{signal:controller.signal,cache:"no-store",credentials:"same-origin"});if(!response.ok)throw new Error("unavailable");return await response.json();}finally{clearTimeout(timer);}
  }
  async function refreshHistory() {
    try {const result=await getJSON("/api/v1/history?seconds="+state.window+"&limit=360&scope=charts");state.historyAvailable=result.available===true;state.history=Array.isArray(result.samples)?result.samples.filter(x=>known(x.at)&&x.at>0).slice(-360).sort((a,b)=>a.at-b.at):[];$("history-status").textContent=state.historyAvailable?(state.history.length?"Rolling 24-hour retention · "+state.history.length+" snapshots in this window":"History is collecting; no snapshots in this window yet."):"Persistent history unavailable. Current measurements may still be available.";}catch{state.history=[];state.historyAvailable=false;$("history-status").textContent="History temporarily unavailable. Current measurements are independent.";}state.historyAt=Date.now();renderHistory();
  }
  async function tick() {
    if(!state.paused) {
      try {const sample=await getJSON("/api/v1/current");if(sample.at!==state.current?.at){state.previous=state.current;state.current=sample;}renderCurrent();status("connection",sample.at>0&&Date.now()-sample.at<45000?"Live · refresh 5s":"Waiting for a fresh sample");}catch{state.current=null;state.previous=null;renderCurrent();status("connection","Disconnected · retrying");}
      if(Date.now()-state.historyAt>=15000)await refreshHistory();
    }
    setTimeout(tick,5000);
  }
  $("pause").addEventListener("click",()=>{state.paused=!state.paused;$("pause").textContent=state.paused?"Resume view":"Pause view";status("connection",state.paused?"View paused · collection continues":"Resuming…");state.historyAt=0;});
  $("window").addEventListener("change",()=>{state.window=Number($("window").value);state.historyAt=0;if(state.paused){$("history-status").textContent="Resume the view to refresh this window.";}});
  tick();
})();
