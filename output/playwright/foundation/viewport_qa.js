// Execute with Chrome DevTools after setting a device viewport.
async () => {
 const pause=ms=>new Promise(r=>setTimeout(r,ms));
 const button=text=>[...document.querySelectorAll('button')].find(e=>e.textContent.trim()===text);
 const click=async text=>{const e=button(text);if(!e)throw Error('missing '+text);e.scrollIntoView({block:'center'});e.click();await pause(180)};
 await click('管理'); await click('Provider 与路由');await pause(180);
 const results={viewport:[innerWidth,innerHeight],views:[]};
 for(const tab of ['Provider 与路由','脱敏策略','历史记录']){
   if(tab!=='Provider 与路由')await click(tab);
   const panel=document.querySelector('[aria-label="网关管理"]');
   const controls=[...panel.querySelectorAll('input,textarea,select,button')].filter(e=>!e.disabled&&e.getBoundingClientRect().width>0&&e.closest('details')?.open!==false);
   const misses=[];
   for(const e of controls){e.scrollIntoView({block:'center'});const rect=e.getBoundingClientRect();const x=rect.x+Math.min(rect.width/2,80),y=rect.y+rect.height/2;const hit=document.elementFromPoint(x,y);if(rect.width>innerWidth||rect.x< -1||rect.right>innerWidth+1||!hit||!(e.contains(hit)||hit.contains(e))){misses.push({label:e.getAttribute('aria-label')||e.textContent.slice(0,35),rect:rect.toJSON(),hit:hit?.tagName})}}
   results.views.push({tab,controls:controls.length,misses,documentOverflow:document.documentElement.scrollWidth>innerWidth+1,panelOverflow:panel.scrollWidth>panel.clientWidth+1});
 }
 await click('Provider 与路由');document.querySelector('[aria-label="网关管理"]').scrollTop=0;await pause(100);
 window.foundationResults??={viewports:[]};window.foundationResults.viewports??=[];window.foundationResults.viewports.push(results);return results;
}
