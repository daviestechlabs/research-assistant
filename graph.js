// Graph drawing and inspection stay in the browser. Only generation calls AI.
let graphBusy=false,graphEnabled=false,graphVersion=0,graphData=null,graphFocus='',graphZoom=1;
const graphNS='http://www.w3.org/2000/svg';
const graphElement=(tag,attributes={},text='')=>{const n=document.createElementNS(graphNS,tag);for(const [k,v] of Object.entries(attributes))n.setAttribute(k,String(v));if(text)n.textContent=text;return n};

function clearGraph(){
 graphVersion++;graphData=null;graphFocus='';el('content').classList.remove('graph-active');
 el('graph-panel').hidden=true;el('graph-canvas').replaceChildren();el('graph-detail').replaceChildren();el('graph-connections').replaceChildren();
 el('graph-json').disabled=true;el('graph-svg').disabled=true;
}
function graphControls(){
 el('generate-graph').disabled=!graphEnabled||!selected.size||[...selected.values()].some(d=>d.source==='dnd')||graphBusy||answerBusy||searchBusy;
 el('generate-graph').textContent=graphBusy?'Mapping and reviewing the abstracts…':'Generate and review knowledge graph';
 el('graph-speculative').disabled=[...selected.values()].some(d=>d.source==='dnd')||graphBusy||answerBusy||searchBusy;
}
function graphEvidence(container,evidence){
 const sources=new Map(graphData.sources.map(d=>[d.id,d]));
 for(const e of evidence){
  const block=node('blockquote',e.quote,'graph-quote'),citation=node('p','');
  const d=sources.get(e.id);if(d)citation.append(sourceLink(d,d.title));
  block.append(citation);container.append(block);
 }
}
function graphPickNode(n){
 graphFocus=n.id;drawGraph();const detail=el('graph-detail');detail.replaceChildren();
 detail.append(node('p',n.kind==='paper'?'Selected source':'Concept from the abstracts','graph-kicker'),node('h3',n.label));
 if(n.kind==='paper'){
  const d=graphData.sources.find(s=>s.id===n.id);
  if(d){detail.append(node('p',d.authors));detail.append(sourceLink(d,d.source==='arxiv'?'Open saved PDF ↗':'Open source ↗'));const full=node('details');full.append(node('summary','Read source abstract'),node('p',d.summary,'full-summary'));detail.append(full)}
 }else{graphEvidence(detail,n.evidence)}
 const adjacent=graphData.edges.filter(e=>(e.from===n.id||e.to===n.id)&&(e.kind!=='hypothesis'||el('graph-show-speculative').checked));
 if(adjacent.length){detail.append(node('h4','Connected links'));for(const e of adjacent){const b=node('button',graphEdgeText(e),'graph-relation-button');b.type='button';b.onclick=()=>graphPickEdge(e);detail.append(b)}}
}
function graphPickEdge(e){
 graphFocus=e.id;drawGraph();const detail=el('graph-detail');detail.replaceChildren();
 detail.append(node('p',e.kind==='hypothesis'?'Speculative connection · novelty unverified':e.type==='mention'?'Verified term occurrence':'AI-extracted reported relation','graph-kicker'),node('h3',graphEdgeText(e)));
 if(e.rationale)detail.append(node('h4','Proposed mechanism and assumptions'),node('p',e.rationale));
 if(e.test)detail.append(node('h4','Suggested test'),node('p',e.test));
 detail.append(node('h4',e.kind==='hypothesis'?'Excerpts ground the concepts; they do not establish this connection.':'Supporting abstract excerpts'));
 graphEvidence(detail,e.evidence);
 if(e.type==='relation'&&e.kind==='reported')detail.append(node('p','Review the excerpts to check whether they support this relationship.','graph-note'));
}
function graphEdgeText(e){
 const nodes=new Map(graphData.nodes.map(n=>[n.id,n]));
 return (nodes.get(e.from)?.label||e.from)+' → '+e.label+' → '+(nodes.get(e.to)?.label||e.to)+(e.kind==='hypothesis'?' (speculative)':'');
}
function graphPickReview(review){
 graphFocus='';drawGraph();const detail=el('graph-detail');detail.replaceChildren();
 detail.append(node('p',review.verdict==='insufficient'?'Relation omitted by evidence review':'Relation retained by evidence review','graph-kicker'),node('h3',graphEdgeText(review.edge)),node('p','Review reason: '+review.reason.replace(/-/g,' '),'graph-note'));
 if(review.edge.rationale)detail.append(node('h4','Proposed mechanism and assumptions'),node('p',review.edge.rationale));
 if(review.edge.test)detail.append(node('h4','Suggested test'),node('p',review.edge.test));
 graphEvidence(detail,review.edge.evidence);
 detail.append(node('p','This local-model review can make mistakes. Check the relationship against these source excerpts.','graph-note'));
}
function graphPickEvidenceReview(review){
 graphFocus='';drawGraph();const detail=el('graph-detail');detail.replaceChildren();
 detail.append(node('p',review.verdict==='insufficient'?'Source excerpt omitted':'Source excerpt retained','graph-kicker'),node('h3',review.reason.replace(/-/g,' ')));
 graphEvidence(detail,[review.evidence]);
 detail.append(node('p','This source classification can make mistakes. It does not prove a scientific relationship.','graph-note'));
}
function graphLines(text,width,maxLines=2){
 const words=text.split(/\s+/),lines=[];let current='';
 for(const word of words){if(current&&current.length+word.length+1>width){lines.push(current);current=word}else current+=(current?' ':'')+word}
 if(current)lines.push(current);const clipped=lines.slice(0,maxLines);
 if(lines.length>maxLines)clipped[maxLines-1]=clipped[maxLines-1].slice(0,width-1)+'…';
 return clipped;
}
function drawGraph(){
 if(!graphData)return;
 const active=document.activeElement,focusID=active?.dataset?.graphId,focusTag=active?.tagName;
 const papers=graphData.nodes.filter(n=>n.kind==='paper'),concepts=graphData.nodes.filter(n=>n.kind==='concept');
 const width=1000,height=Math.max(430,80+papers.length*90,100+Math.ceil(concepts.length/2)*110),positions=new Map();
 papers.forEach((n,i)=>positions.set(n.id,{x:145,y:85+i*(height-135)/Math.max(1,papers.length-1),w:230,h:66}));
 concepts.forEach((n,i)=>positions.set(n.id,{x:565+(i%2)*270,y:85+Math.floor(i/2)*110,w:210,h:66}));
 const svg=graphElement('svg',{viewBox:'0 0 '+width+' '+height,width:width*graphZoom,height:height*graphZoom,role:'group','aria-label':'Knowledge graph for '+graphData.query});
 const defs=graphElement('defs');
 for(const [id,color] of [['graph-arrow','#799088'],['graph-hypothesis-arrow','#b96f15']]){const marker=graphElement('marker',{id,viewBox:'0 0 10 10',refX:9,refY:5,markerWidth:7,markerHeight:7,orient:'auto-start-reverse'});marker.append(graphElement('path',{d:'M 0 0 L 10 5 L 0 10 z',fill:color}));defs.append(marker)}
 svg.append(defs,graphElement('rect',{width,height,fill:'#fff'}));
 svg.append(graphElement('text',{x:30,y:25,fill:'#687078','font-family':'system-ui, sans-serif','font-size':13},'Selected sources'));
 svg.append(graphElement('text',{x:460,y:25,fill:'#687078','font-family':'system-ui, sans-serif','font-size':13},'Concepts from their abstracts'));
 const focusedEdge=graphData.edges.find(e=>e.id===graphFocus);
 const connected=new Set([graphFocus]);if(focusedEdge){connected.add(focusedEdge.from);connected.add(focusedEdge.to)}
 for(const e of graphData.edges){if(e.from===graphFocus||e.to===graphFocus){connected.add(e.from);connected.add(e.to)}}
 const visibleEdges=graphData.edges.filter(e=>e.kind!=='hypothesis'||el('graph-show-speculative').checked);
 for(const e of visibleEdges){
  const a=positions.get(e.from),b=positions.get(e.to);if(!a||!b)continue;
  let x1=a.x+a.w/2,x2=b.x-b.w/2,y1=a.y,y2=b.y;
  if(a.x>b.x){x1=a.x-a.w/2;x2=b.x+b.w/2}
  let d='M '+x1+' '+y1+' C '+((x1+x2)/2)+' '+y1+', '+((x1+x2)/2)+' '+y2+', '+x2+' '+y2;
  if(e.type==='mention'&&b.x>700){const channelX=340+(visibleEdges.indexOf(e)%7)*11,channelY=b.y-48;x2=b.x;y2=b.y-b.h/2;d='M '+x1+' '+y1+' C '+channelX+' '+y1+', '+channelX+' '+channelY+', '+channelX+' '+channelY+' L '+x2+' '+channelY+' L '+x2+' '+y2}
  if(a.x===b.x){x1=a.x+a.w/2;x2=b.x+b.w/2;d='M '+x1+' '+y1+' C '+(x1+60)+' '+y1+', '+(x2+60)+' '+y2+', '+x2+' '+y2}
  const color=e.kind==='hypothesis'?'#b96f15':'#799088',active=!graphFocus||e.id===graphFocus||e.from===graphFocus||e.to===graphFocus;
  const group=graphElement('g',{class:'graph-interactive','data-graph-id':e.id,tabindex:0,role:'button','aria-label':graphEdgeText(e),opacity:active?1:.16});
  group.append(graphElement('title',{},graphEdgeText(e)),graphElement('path',{d,fill:'none',stroke:color,'stroke-width':e.id===graphFocus?4:2,'stroke-dasharray':e.kind==='hypothesis'?'7 5':'none','marker-end':'url(#'+(e.kind==='hypothesis'?'graph-hypothesis-arrow':'graph-arrow')+')'}),graphElement('path',{d,fill:'none',stroke:'transparent','stroke-width':18,'pointer-events':'stroke'}));
  if(e.type==='relation'&&el('graph-labels').checked){const x=Math.min(width-95,Math.max(95,(x1+x2)/2)),y=(y1+y2)/2;group.append(graphElement('rect',{x:x-83,y:y-12,width:166,height:24,rx:6,fill:'#fff'}),graphElement('text',{x,y:y+4,'text-anchor':'middle','font-size':12,'font-family':'system-ui, sans-serif',fill:color},e.label.length>26?e.label.slice(0,25)+'…':e.label))}
  group.onclick=()=>graphPickEdge(e);group.onkeydown=event=>{if(event.key==='Enter'||event.key===' '){event.preventDefault();graphPickEdge(e)}};svg.append(group);
 }
 for(const n of graphData.nodes){
  const p=positions.get(n.id),paper=n.kind==='paper',fill=paper?'#eaf2ff':'#edf5ea',stroke=paper?'#91b3e5':'#a8c396';
  const group=graphElement('g',{class:'graph-interactive','data-graph-id':n.id,tabindex:0,role:'button','aria-label':(paper?'Source: ':'Concept: ')+n.label,opacity:!graphFocus||connected.has(n.id)?1:.48});
  group.append(graphElement('title',{},n.label),graphElement('rect',{x:p.x-p.w/2,y:p.y-p.h/2,width:p.w,height:p.h,rx:paper?8:22,fill,stroke:n.id===graphFocus?'#215d51':stroke,'stroke-width':n.id===graphFocus?3:1.5}));
  const lines=graphLines(n.label,paper?31:27,2);
  lines.forEach((line,i)=>group.append(graphElement('text',{x:p.x,y:p.y-(lines.length-1)*9+i*18+5,'text-anchor':'middle',fill:'#202124','font-size':14,'font-family':'system-ui, sans-serif'},line)));
  group.onclick=()=>graphPickNode(n);group.onkeydown=event=>{if(event.key==='Enter'||event.key===' '){event.preventDefault();graphPickNode(n)}};svg.append(group);
 }
 const scroller=el('graph-canvas'),left=scroller.scrollLeft,top=scroller.scrollTop;scroller.replaceChildren(svg);scroller.scrollLeft=left;scroller.scrollTop=top;
 el('graph-zoom').textContent=Math.round(graphZoom*100)+'%';
 el('graph-status').textContent=graphData.sources.length+' sources · '+concepts.length+' concepts · '+visibleEdges.length+' visible links · '+(graphData.elapsed_ms/1000).toFixed(1)+' seconds · '+(graphData.cached?'cached result · no new AI call':(graphData.model_calls||1)+' AI call'+(graphData.model_calls===1?'':'s'))+(graphData.omitted_items?' · '+graphData.omitted_items+' unsupported items omitted':'')+(graphData.unmapped_sources?.length?' · '+graphData.unmapped_sources.length+' sources without verified concept links':'');
 const list=el('graph-connections'),reviewOpen=!!list.querySelector('[data-graph-review]')?.open;list.replaceChildren();
 for(const e of visibleEdges){const li=document.createElement('li'),button=node('button',graphEdgeText(e),'graph-relation-button');button.type='button';button.dataset.graphId=e.id;button.onclick=()=>graphPickEdge(e);li.append(button);list.append(li)}
 if(graphData.relation_review?.length){const li=document.createElement('li'),details=node('details'),summary=node('summary','Inspect '+graphData.relation_review.length+' relation review decision'+(graphData.relation_review.length===1?'':'s'));details.dataset.graphReview='';details.open=reviewOpen;details.append(summary);for(const review of graphData.relation_review){const button=node('button',(review.verdict==='insufficient'?'Omitted: ':'Retained: ')+graphEdgeText(review.edge),'graph-relation-button');button.type='button';button.dataset.graphId='review:'+review.edge_id;button.onclick=()=>graphPickReview(review);details.append(button)}li.append(details);list.append(li)}
 if(graphData.evidence_review?.length){const li=document.createElement('li'),details=node('details');details.append(node('summary','Inspect '+graphData.evidence_review.length+' source evidence decisions'));for(const review of graphData.evidence_review){const button=node('button',(review.verdict==='insufficient'?'Omitted: ':'Retained: ')+review.evidence.id+' · '+review.reason.replace(/-/g,' '),'graph-relation-button');button.type='button';button.onclick=()=>graphPickEvidenceReview(review);details.append(button)}li.append(details);list.append(li)}
 if(focusID){for(const element of el('graph-panel').querySelectorAll('[data-graph-id]')){if(element.dataset.graphId===focusID&&element.tagName===focusTag){element.focus({preventScroll:true});break}}}
}
function fitGraph(){
 if(!graphData)return;
 const canvas=el('graph-canvas'),box=canvas.querySelector('svg').viewBox.baseVal;
 graphZoom=Math.min(1,Math.max(.3,Math.min(canvas.clientWidth/box.width,canvas.clientHeight/box.height)));drawGraph();
}
function downloadGraph(kind){
 if(!graphData)return;
 const content=kind==='json'?JSON.stringify(graphData,null,2):new XMLSerializer().serializeToString(el('graph-canvas').querySelector('svg'));
 const blob=new Blob([content],{type:kind==='json'?'application/json':'image/svg+xml'}),url=URL.createObjectURL(blob),a=document.createElement('a');
 a.href=url;a.download='research-desk-'+graphData.query.toLowerCase().replace(/[^a-z0-9]+/g,'-').slice(0,60)+'.'+kind;document.body.append(a);a.click();a.remove();setTimeout(()=>URL.revokeObjectURL(url),60000);
}
function initGraph(){
 el('generate-graph').onclick=async()=>{
  if(!graphEnabled||!selected.size||[...selected.values()].some(d=>d.source==='dnd')||graphBusy||answerBusy||searchBusy)return;
  clearGraph();graphBusy=true;const version=graphVersion;controls();
  el('graph-panel').hidden=false;el('content').classList.add('graph-active');el('graph-status').textContent='Mapping the selected abstracts…';el('graph-topic').textContent=el('question').value.trim()||el('query').value.trim();
  el('graph-panel').scrollIntoView({behavior:'smooth',block:'start'});
  try{
   const value=await api('/api/graph',{query:el('question').value.trim()||el('query').value.trim(),ids:[...selected.keys()],include_hypotheses:el('graph-speculative').checked});
   if(version!==graphVersion)return;
   graphData=value;graphFocus='';graphZoom=1;
   el('graph-topic').textContent=value.query;el('graph-limits').textContent=value.limits;
   el('graph-show-speculative').checked=true;el('graph-speculative-display').hidden=!value.edges.some(e=>e.kind==='hypothesis');
   el('graph-json').disabled=false;el('graph-svg').disabled=false;
   drawGraph();fitGraph();el('graph-detail').append(node('p','Select a node or link to inspect its source excerpts.','graph-note'));el('graph-panel').scrollIntoView({behavior:'smooth',block:'start'});
  }catch(error){if(version===graphVersion)el('graph-status').textContent=error.message}
  finally{graphBusy=false;controls();stats()}
 };
 el('graph-speculative').onchange=()=>clearGraph();
 el('graph-show-speculative').onchange=()=>{graphFocus='';el('graph-detail').replaceChildren(node('p','Select a visible node or link to inspect its source excerpts.','graph-note'));drawGraph()};
 el('graph-labels').onchange=()=>drawGraph();
 el('graph-zoom-in').onclick=()=>{graphZoom=Math.min(2,graphZoom+.2);drawGraph()};
 el('graph-zoom-out').onclick=()=>{graphZoom=Math.max(.3,graphZoom-.2);drawGraph()};
 el('graph-fit').onclick=fitGraph;
 el('graph-reset').onclick=()=>{graphFocus='';el('graph-detail').replaceChildren(node('p','Select a node or link to inspect its source excerpts.','graph-note'));drawGraph()};
 el('graph-json').onclick=()=>downloadGraph('json');el('graph-svg').onclick=()=>downloadGraph('svg');
}
