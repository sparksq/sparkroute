// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ManagedConfigurationWorkspace } from "./ManagedConfigurationWorkspace";
import type { AdminBootstrap } from "./types";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const bootstrap: AdminBootstrap = {schema_version:1,edition:"standalone",config_revision:"active",features:{config_read:true,config_write:true,sparkrun_catalog:true}};
const deployment = {name:"saved-target",title:"sparkrun:lab:coder",provider:"sparkrun",model:"test/model",endpoint_source:{type:"activatable",controller:"sparkrun",recipe:"catalog:one",recipe_revision:"saved",revision:"binding",cluster_candidates:["lab"]}};
const initial = {providers:[{name:"sparkrun",type:"sparkrun"}],deployments:[deployment,{name:"running-target",provider:"sparkrun",model:"test/model",endpoint_source:{type:"discovered",controller:"sparkrun"}}],virtual_models:[{name:"coding",aliases:["code"],pools:[{priority:0,targets:[{deployment:"saved-target",weight:1}]}]}]};
const detail={reference:"catalog:one",name:"@official/coder",recipe_revision:"current",model:"test/model",runtime:"vllm",min_nodes:1,source_path:"/recipes/coder.yaml",native_protocols:["openai"],required_plugins:[],defaults:{},issues:[]};
const json=(value:unknown,status=200)=>new Response(JSON.stringify(value),{status,headers:{"Content-Type":"application/json"}});

it("opens review from validation, preserves the fallback draft, and saves only after explicit apply",async()=>{
 let stored=structuredClone(initial); let adopted=false; const saves:any[]=[]; const preparations:any[]=[];
 vi.stubGlobal("fetch",vi.fn(async(path:string,request?:RequestInit)=>{
  const method=request?.method??"GET";const body=request?.body?JSON.parse(String(request.body)):{};
  if(path==="/v1/config/managed-sets")return json({active_revision:"active",presets_revision:1,managed_sets:[{owner:"operator",revision:"active"}]});
  if(path==="/v1/config/managed-sets/operator"&&method==="GET")return json({owner:"operator",revision:"active",document:stored});
  if(path==="/v1/config")return json({revision:"active",document:stored});
  if(path.endsWith("/validate"))return adopted?json({valid:true,candidate_revision:"next"}):json({error:{code:"recipe_revision_changed",deployment:"saved-target",message:"Recipe @official/coder on lab changed since it was reviewed."}},422);
  if(path==="/v1/config/managed-sets/operator"&&method==="PUT"){saves.push(body);stored=body.document;return json({changed:true});}
  if(path==="/v1/sparkrun/recipe-draft"){
   preparations.push(body);adopted=true;
   const document=structuredClone(body.document);document.deployments[0].endpoint_source.recipe_revision=body.recipe.recipe_revision;
   return json({document,deployment:"saved-target",reused:false});
  }
  if(path==="/v1/sparkrun/catalog")switch(body.operation){
   case "catalog_resolve":return json(detail);
   case "catalog_clusters":return json({clusters:[{name:"lab",host_count:2,default:true}]});
   case "catalog_search":return json({recipes:[],total:0,next_offset:null,unavailable_registries:[]});
   case "catalog_registries":return json({registries:[]});
   case "catalog_plugins":return json({plugins:[]});
  }
  return json({error:{message:"not available"}},404);
 }));
 render(<ManagedConfigurationWorkspace bootstrap={bootstrap} token="test" section="models"/>);
 await screen.findByDisplayValue("coding");
 fireEvent.click(screen.getByRole("button",{name:"Add fallback pool"}));
 expect(screen.getByLabelText("Prefer running targets with capacity")).toBeChecked();
 fireEvent.click(screen.getByLabelText("Prefer running targets with capacity"));
 fireEvent.click(screen.getByRole("button",{name:"Validate"}));
 const notices=await screen.findAllByRole("status");
 const error=notices.find(notice=>notice.textContent?.includes("changed since it was reviewed"))!;
 fireEvent.click(within(error).getByRole("button",{name:"Review recipe update"}));
 expect(await screen.findByText("current",{selector:"code"})).toBeVisible();
 expect(preparations).toHaveLength(0);expect(saves).toHaveLength(0);
 fireEvent.click(screen.getByRole("button",{name:"Apply to draft"}));
 await waitFor(()=>expect(screen.queryByRole("button",{name:"Apply to draft"})).not.toBeInTheDocument());
 expect(preparations[0].document.virtual_models[0]).toMatchObject({aliases:["code"],selection:{prefer_running:false},pools:[{priority:0,targets:[{deployment:"saved-target",weight:1}]},{priority:1,targets:[{deployment:"running-target",weight:100}]}]});
 fireEvent.click(screen.getByRole("button",{name:"Validate"}));
 fireEvent.click(await screen.findByRole("button",{name:"Save"}));
 await waitFor(()=>expect(saves).toHaveLength(1));
 expect(saves[0].document.deployments[0].endpoint_source.recipe_revision).toBe("current");
 expect(saves[0].document.virtual_models).toEqual(preparations[0].document.virtual_models);
});
