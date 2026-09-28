package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

const spiritNetRawCatalog = "https://raw.githubusercontent.com/spiritLHLS/speedtest.net-CN-ID/main/CN.csv"
var spiritNetCatalogURLs = []string{
	"https://cdn.spiritlhl.net/" + spiritNetRawCatalog,
	spiritNetRawCatalog,
}
const sukkaNetCatalogURL = "https://speedtest-net-servers.cdn.skk.moe/servers.json"

type supplementalNetNode struct {
	ID, Name, Sponsor, Province, Carrier, URL, Host string
	Custom bool
}

var vpsDanceOoklaNodes = []supplementalNetNode{
	{ID:"http:net:vkit:sh-ct", Name:"上海", Sponsor:"上海电信", Province:"上海", Carrier:"中国电信", Host:"speedtest1.online.sh.cn:8080", Custom:true},
	{ID:"http:net:vkit:suzhou-ct", Name:"苏州", Sponsor:"江苏电信", Province:"江苏", Carrier:"中国电信", Host:"4gsuzhou1.speedtest.jsinfo.net:8080", Custom:true},
	{ID:"http:net:vkit:zhenjiang-ct", Name:"镇江", Sponsor:"江苏电信", Province:"江苏", Carrier:"中国电信", Host:"5gzhenjiang.speedtest.jsinfo.net:8080", Custom:true},
	{ID:"http:net:vkit:nanjing-ct", Name:"南京", Sponsor:"江苏电信", Province:"江苏", Carrier:"中国电信", Host:"5gnanjing.speedtest.jsinfo.net:8080", Custom:true},
	{ID:"http:net:vkit:sh-cu", Name:"上海", Sponsor:"上海联通", Province:"上海", Carrier:"中国联通", Host:"5g.shunicomtest.com:8080", Custom:true},
	{ID:"http:net:vkit:chengdu-cu", Name:"成都", Sponsor:"四川联通", Province:"四川", Carrier:"中国联通", Host:"cuscspeed.169ol.com:8080", Custom:true},
	{ID:"http:net:vkit:fuzhou-cm", Name:"福州", Sponsor:"福建移动", Province:"福建", Carrier:"中国移动", Host:"csfw.fj.chinamobile.com:8080", Custom:true},
	{ID:"http:net:vkit:hangzhou-cm", Name:"杭州", Sponsor:"浙江移动", Province:"浙江", Carrier:"中国移动", Host:"speedtest.139play.com:8080", Custom:true},
	{ID:"http:net:vkit:beijing-cm", Name:"北京", Sponsor:"北京移动", Province:"北京", Carrier:"中国移动", Host:"speedtest.bmcc.com.cn:8080", Custom:true},
}

func fetchLimited(ctx context.Context, client *http.Client, address string, limit int64) ([]byte,error){
	req,err:=http.NewRequestWithContext(ctx,http.MethodGet,address,nil)
	if err!=nil{return nil,err}
	req.Header.Set("User-Agent","fnOS-Speedtest/"+appVersion)
	resp,err:=client.Do(req)
	if err!=nil{return nil,err}
	defer resp.Body.Close()
	if resp.StatusCode!=http.StatusOK{return nil,fmt.Errorf("HTTP %d",resp.StatusCode)}
	b,err:=io.ReadAll(io.LimitReader(resp.Body,limit+1))
	if err!=nil{return nil,err}
	if int64(len(b))>limit{return nil,errors.New("catalog too large")}
	return b,nil
}

func supplementalProvince(name, sponsor string) string {
	s := &speedtest.Server{Name:name, Country:"China", CC:"CN", Sponsor:sponsor}
	if province := nodeProvince(s); province != "" {
		return province
	}
	text := strings.ToLower(name + " " + sponsor)
	for _, item := range []struct{
		province string
		keys []string
	}{
		{"江苏", []string{"suzhou","zhenjiang","jiangsu"}},
		{"福建", []string{"fuzhou","fujian"}},
		{"浙江", []string{"ningbo","zhejiang"}},
		{"四川", []string{"chengdu","sichuan"}},
		{"上海", []string{"shanghai"}},
		{"北京", []string{"beijing"}},
	} {
		for _, key := range item.keys {
			if strings.Contains(text, key) {
				return item.province
			}
		}
	}
	return ""
}

func parseSpiritNetCatalog(data []byte)([]supplementalNetNode,error){
	r:=csv.NewReader(strings.NewReader(string(data))); r.FieldsPerRecord=-1
	header,err:=r.Read(); if err!=nil{return nil,err}
	cols:=map[string]int{}
	for i,v:=range header{cols[strings.ToLower(strings.TrimSpace(v))]=i}
	for _,k:=range []string{"id","country_code","country","city","host","port","supplier"}{if _,ok:=cols[k];!ok{return nil,fmt.Errorf("missing %s",k)}}
	get:=func(row []string,k string)string{i:=cols[k];if i>=len(row){return ""};return strings.TrimSpace(row[i])}
	out:=[]supplementalNetNode{}; seen:=map[string]bool{}
	for{
		row,e:=r.Read(); if errors.Is(e,io.EOF){break}; if e!=nil{return nil,e}
		if !strings.EqualFold(get(row,"country_code"),"CN") || !strings.EqualFold(get(row,"country"),"China"){continue}
		id,host,port:=get(row,"id"),get(row,"host"),get(row,"port")
		if id==""||host==""||port==""||seen[id]{continue}
		seen[id]=true
		name:=strings.TrimSuffix(get(row,"city"),"市"); sponsor:=get(row,"supplier")
		s:=&speedtest.Server{Name:name,Country:"China",CC:"CN",Sponsor:sponsor}
		out=append(out,supplementalNetNode{ID:id,Name:name,Sponsor:sponsor,Province:supplementalProvince(name,sponsor),Carrier:canonicalCarrier(sponsor),Host:host+":"+port,URL:"http://"+host+":"+port+"/speedtest/upload.php"})
	}
	if len(out)==0{return nil,errors.New("empty spirit net catalog")}
	return out,nil
}

func fetchSpiritNetCatalog(ctx context.Context, client *http.Client)([]supplementalNetNode,error){
	type result struct{nodes []supplementalNetNode;err error}
	ctx,cancel:=context.WithCancel(ctx); defer cancel()
	ch:=make(chan result,len(spiritNetCatalogURLs))
	for _,address:=range spiritNetCatalogURLs{go func(a string){b,e:=fetchLimited(ctx,client,a,1<<20);if e==nil{var n []supplementalNetNode;n,e=parseSpiritNetCatalog(b);ch<-result{n,e};return};ch<-result{err:e}}(address)}
	var last error
	for range spiritNetCatalogURLs{select{case x:=<-ch:if x.err==nil&&len(x.nodes)>0{cancel();return x.nodes,nil};last=x.err;case<-ctx.Done():return nil,ctx.Err()}}
	return nil,last
}

func parseSukkaNetCatalog(data []byte)([]supplementalNetNode,error){
	var rows []struct{URL,Name,Country,CC,Sponsor,ID,Host string}
	if err:=json.Unmarshal(data,&rows);err!=nil{return nil,err}
	out:=[]supplementalNetNode{}; seen:=map[string]bool{}
	for _,x:=range rows{
		if !strings.EqualFold(x.CC,"CN")||x.ID==""||x.URL==""||seen[x.ID]{continue}
		u,err:=url.Parse(x.URL);if err!=nil||u.Hostname()==""{continue}
		seen[x.ID]=true
		s:=&speedtest.Server{Name:x.Name,Country:x.Country,CC:x.CC,Sponsor:x.Sponsor}
		out=append(out,supplementalNetNode{ID:x.ID,Name:x.Name,Sponsor:x.Sponsor,Province:supplementalProvince(x.Name,x.Sponsor),Carrier:canonicalCarrier(x.Sponsor),Host:x.Host,URL:x.URL})
	}
	if len(out)==0{return nil,errors.New("empty sukka net catalog")}
	return out,nil
}

func fetchSukkaNetCatalog(ctx context.Context, client *http.Client)([]supplementalNetNode,error){
	b,err:=fetchLimited(ctx,client,sukkaNetCatalogURL,8<<20);if err!=nil{return nil,err}
	return parseSukkaNetCatalog(b)
}

func customOoklaTarget(node supplementalNetNode,n networkIdentity)(httpTarget,error){
	custom,err:=speedtestCNCustomURL(node.Host);if err!=nil{return httpTarget{},err}
	src:=httpSource{ID:node.ID,Name:node.Name,Sponsor:node.Sponsor,Province:node.Province,Carrier:node.Carrier,Kind:"speedtest",Page:"https://github.com/VPSDance/vkit"}
	return httpTarget{Protocol:"speedtestnet",Version:"1",Source:src,Network:n,CustomURL:custom},nil
}

func supplementalProbeServer(node supplementalNetNode)(*speedtest.Server,error){
	address:=node.URL
	if node.Custom{
		target,err:=customOoklaTarget(node,networkIdentity{});if err!=nil{return nil,err}
		u,err:=url.Parse(target.CustomURL);if err!=nil{return nil,err}
		u.Path="/speedtest/upload.php";address=u.String()
	}
	if address==""{return nil,errors.New("missing upload url")}
	return &speedtest.Server{ID:node.ID,Name:node.Name,Country:"China",CC:"CN",Sponsor:node.Sponsor,URL:address},nil
}

func (m *multiEngine) discoverSupplementalNet(ctx context.Context,n networkIdentity)[]serverOption{
	type feed struct{nodes []supplementalNetNode}
	ch:=make(chan feed,2)
	go func(){nodes,_:=fetchSpiritNetCatalog(ctx,m.client);ch<-feed{nodes}}()
	go func(){nodes,_:=fetchSukkaNetCatalog(ctx,m.client);ch<-feed{nodes}}()
	all:=append([]supplementalNetNode{},vpsDanceOoklaNodes...)
	for i:=0;i<2;i++{select{case x:=<-ch:all=append(all,x.nodes...);case<-ctx.Done():return nil}}

	// Prefer live external metadata over curated duplicates, dedupe by numeric ID and host.
	seenID:=map[string]bool{};seenHost:=map[string]bool{};dedup:=make([]supplementalNetNode,0,len(all))
	sort.SliceStable(all,func(i,j int)bool{return !all[i].Custom&&all[j].Custom})
	for _,node:=range all{
		host:=strings.ToLower(strings.TrimSpace(node.Host))
		if host==""&&node.URL!=""{if u,e:=url.Parse(node.URL);e==nil{host=strings.ToLower(u.Host)}}
		if seenID[node.ID]||(host!=""&&seenHost[host]){continue}
		seenID[node.ID]=true;if host!=""{seenHost[host]=true};dedup=append(dedup,node)
	}
	type checked struct{node supplementalNetNode;lat float64;ok bool}
	jobs:=make(chan supplementalNetNode,len(dedup));results:=make(chan checked,len(dedup))
	for _,x:=range dedup{jobs<-x};close(jobs)
	workers:=min(24,len(dedup));var wg sync.WaitGroup;wg.Add(workers)
	for range workers{go func(){defer wg.Done();for node:=range jobs{
		s,err:=supplementalProbeServer(node);if err!=nil{results<-checked{node:node};continue}
		pctx,cancel:=context.WithTimeout(ctx,900*time.Millisecond);err=probeServer(pctx,m.client,s);cancel()
		if err==nil{
			if node.Custom{if target,e:=customOoklaTarget(node,n);e==nil{m.mu.Lock();m.targets[node.ID]=verifiedTarget{target:target,expires:time.Now().Add(30*time.Minute)};m.mu.Unlock()}}
			results<-checked{node:node,lat:float64(s.Latency)/float64(time.Millisecond),ok:true}
		}else{results<-checked{node:node}}
	}}()}
	go func(){wg.Wait();close(results)}()
	out:=[]serverOption{}
	for x:=range results{
		if !x.ok{continue}
		node:=x.node
		out=append(out,serverOption{ID:node.ID,Name:node.Name,Country:"中国",Sponsor:node.Sponsor,Carrier:node.Carrier,Province:node.Province,ProvinceMatched:n.CountryCode=="CN"&&n.Province!=""&&n.Province==node.Province,CarrierMatched:n.Carrier!=""&&n.Carrier==node.Carrier,LatencyMS:round2(x.lat),LatencyMeasured:true,Mainland:true,Kind:"speedtest",Engine:"Speedtest.net"})
	}
	return out
}
