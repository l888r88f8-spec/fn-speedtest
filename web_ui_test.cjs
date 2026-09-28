// Dependency-free DOM harness: node web_ui_test.cjs
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const path = require('node:path');
class Element {
 constructor() {
  this.textContent='';this.value='';this.style={};this.children=[];this.disabled=false;this.events={};
  this.classes=new Set();
  this.classList={toggle:(name,enabled)=>enabled ? this.classes.add(name) : this.classes.delete(name)};
 }
 replaceChildren(...items) { this.children=items;this.value=items[0]?.value||''; }
 append(item) {this.children.push(item)}
 addEventListener(event,fn) {this.events[event]=fn}
 click() {if(!this.disabled) return this.events.click()}
}
(async () => {
 const html=fs.readFileSync(path.join(__dirname,'web/index.html'),'utf8');
 const elements={};
 for(const [,id] of html.matchAll(/id="([^"]+)"/g)) elements[id]=new Element();
 assert.equal(elements.durationValue,undefined);assert.equal(elements.cancelButton,undefined);assert.equal(elements.notice,undefined);
 elements.profile.value='standard';
 const document={getElementById:id=>elements[id],createElement:()=>new Element(),querySelectorAll:()=>[elements.downloadValue,elements.uploadValue,elements.latencyValue]};
 let catalogueMode='normal';
 let state={status:'idle'},starts=0,catalogs=0,chosen='',releaseStart,releaseState,releaseCancel,holdState=false;
 const fetch=async (url,options={})=> {
  const endpoint=url.slice(4);let data={};
  if(endpoint==='info') data={version:'1.10.8'};
  if(endpoint==='history') data=null; // Empty history must still render without map errors.
  if(endpoint==='servers') {catalogs++;data={publicIp:'114.114.114.114',network:{publicIp:'114.114.114.114',carrier:'中国电信',countryCode:'CN',province:'江苏'},recommendedId:'http:cn:1',sources:[{id:'speedtestcn',status:'available'}],servers:[
   {id:'http:cn:1',name:'南京',province:'江苏',country:'中国',sponsor:'中国电信',carrier:'中国电信',kind:'speedtestcn',engine:'Speedtest.cn',mainland:true,provinceMatched:true,carrierMatched:true,latencyMeasured:true,latencyMs:8,jitterMs:.3,recommended:true},
   {id:'http:cn:2',name:'杭州',province:'浙江',country:'中国',sponsor:'中国电信',carrier:'中国电信',kind:'speedtestcn',engine:'Speedtest.cn',mainland:true,carrierMatched:true,latencyMeasured:true,latencyMs:10,jitterMs:.4},
   {id:'http:cn:3',name:'苏州',province:'江苏',country:'中国',sponsor:'中国联通',carrier:'中国联通',kind:'speedtestcn',engine:'Speedtest.cn',mainland:true,provinceMatched:true,latencyMeasured:true,latencyMs:11,jitterMs:.5},
   {id:'http:cn:4',name:'成都',province:'四川',country:'中国',sponsor:'中国移动',carrier:'中国移动',kind:'speedtestcn',engine:'Speedtest.cn',mainland:true,latencyMeasured:true,latencyMs:16,jitterMs:.6},
   {id:'cn1',name:'上海',country:'中国',sponsor:'中国联通',mainland:true,latencyMs:8,jitterMs:.3},
   {id:'cn2',name:'北京',country:'中国',sponsor:'中国电信',mainland:true,latencyMs:16,jitterMs:.6},
   {id:'http:edu',name:'高校',sponsor:'大学',kind:'university',mainland:true,latencyMs:12},
   {id:'http:isp',name:'运营商',sponsor:'电信',kind:'operator',mainland:true,latencyMs:11},
   {id:'near',name:'Tokyo',country:'Japan',sponsor:'ISP',mainland:false,latencyMs:90,jitterMs:2}
  ]};}
  if(endpoint==='servers' && catalogueMode==='recommended') {data.recommendedId='http:cn:1'; data.servers=data.servers.filter(s=>s.id==='http:cn:1');data.servers[0].recommended=true;data.sources=[{id:'speedtestcn',status:'available'}];}
  if(endpoint==='servers' && ['failed','empty'].includes(catalogueMode)) {
   data.sources=[{id:'speedtestcn',status:'directory_error',message:'节点目录获取失败：连接超时'}];
   data.servers=data.servers.filter(s=>s.kind!=='speedtestcn');
   data.recommendedId='cn1';data.servers[0].recommended=true;
   if(catalogueMode==='empty'){data.error='没有可用节点';data.servers=[];}
  }
  if(endpoint==='state') {
   data={...state};
   if(holdState) {holdState=false;data={...state,liveDownloadMbps:999,downloadProgress:100};await new Promise(r=>releaseState=r)}
  }
  if(endpoint==='test') {
   starts++;chosen=JSON.parse(options.body).serverId;
   await new Promise(r=>releaseStart=r);
   state={status:'running',phase:'download',progress:60,liveDownloadMbps:123.45,downloadProgress:42};
  }
  if(endpoint==='test/cancel') {await new Promise(r=>releaseCancel=r);state={...state,status:'cancelling'}}
  return {ok:!(endpoint==='servers' && catalogueMode==='empty'),status:(endpoint==='servers' && catalogueMode==='empty')?502:200,text:async()=>JSON.stringify(data)};
 };
 const context=vm.createContext({document,window:{addEventListener(){}},fetch,Option:class extends Element {constructor(label,value){super();this.textContent=label;this.value=value}},setInterval:()=>1,clearInterval(){},confirm:()=>true,console});
 vm.runInContext(fs.readFileSync(path.join(__dirname,'web/app.js'),'utf8'),context);
 const flush=()=>new Promise(r=>setImmediate(r));
 await flush();
 assert.equal(elements.startButton.disabled,false);
 assert.equal(elements.ispValue.textContent,'中国电信');
 assert.equal(elements.regionValue.textContent,'江苏 · IP 归属地');
 assert.equal(elements.serverSelect.children.find(x=>x.label==='Speedtest.cn · 同省同运营商'),undefined); // Recommended item is not duplicated.
 assert.equal(elements.serverSelect.children.find(x=>x.label==='Speedtest.cn · 同运营商其他省').children[0].value,'http:cn:2');
 assert.equal(elements.serverSelect.children.find(x=>x.label==='Speedtest.cn · 本省其他网络').children[0].value,'http:cn:3');
 assert.equal(elements.serverSelect.children.find(x=>x.label==='Speedtest.cn · 其他中国节点').children[0].value,'http:cn:4');
 assert.equal(elements.serverSelect.children.find(x=>x.label==='Speedtest.net · 中国境内').children.length,2);
 assert.equal(elements.serverSelect.children.find(x=>x.label==='高校节点').children[0].value,'http:edu');
	 assert.equal(elements.serverSelect.children.find(x=>x.label==='公开运营商节点').children[0].value,'http:isp');
 assert.equal(elements.serverSelect.children.some(x=>x.label==='全球网测节点'),false);
 elements.serverSelect.value='http:cn:2';
 const starting=elements.startButton.click();
 assert.equal(elements.startButton.disabled,true);
 elements.startButton.click();assert.equal(starts,1);
 releaseStart();await starting;
 assert.equal(elements.actionLabel.textContent,'取消测速');
 assert.equal(elements.downloadValue.textContent,'123.45');
 assert.equal(chosen,'http:cn:2');assert.equal(catalogs,1);
 holdState=true;
 const stalePoll=context.refreshState();
 const cancelling=elements.startButton.click();
 assert.equal(elements.actionLabel.textContent,'正在停止…');
 assert.equal(elements.startButton.disabled,true);
 assert.equal(elements.downloadValue.classes.has('pulse'),false);
 releaseState();await stalePoll;
 assert.equal(elements.downloadValue.textContent,'123.45');
 assert.equal(elements.downloadProgressBar.style.width,'42%');
 releaseCancel();await cancelling;
 assert.equal(elements.startButton.disabled,true);
 state={status:'cancelled'};await context.refreshState();
 assert.equal(elements.actionLabel.textContent,'开始测速');
 assert.equal(elements.startButton.disabled,false);
 assert.equal(elements.downloadValue.textContent,'123.45');
 state={status:'error',message:'节点连接失败'};await context.refreshState();
 assert.equal(elements.actionError.textContent,'节点连接失败');
 context.showResult({publicIp:'8.8.8.8',isp:'Other ISP',downloadMbps:1,uploadMbps:1,latencyMs:1,jitterMs:1,durationSec:1});
 assert.equal(elements.regionValue.textContent,'省份未识别');
 catalogueMode='recommended';await context.loadServers();
 assert.ok(elements.serverSelect.children[0].textContent.includes('Speedtest.cn'));
 assert.equal(elements.serverSelect.children.some(x=>x.label==='全球网测节点'),false);
 catalogueMode='failed';await context.loadServers();
 assert.equal(elements.sourceHint.textContent,'Speedtest.cn：节点目录获取失败：连接超时');
	assert.equal(elements.startButton.disabled,false);
	catalogueMode='empty';await context.loadServers();
 assert.equal(elements.sourceHint.textContent,'Speedtest.cn：节点目录获取失败：连接超时');
 assert.equal(elements.startButton.disabled,true);
 console.log('PASS: Speedtest.cn source diagnostics and clear match groups; GlobalSpeed hidden; selected ID, one start request, no repeat discovery, immediate cancel freeze, stale poll suppression, stopped/error states, empty history');
})().catch(error=>{console.error(error);process.exit(1)});
