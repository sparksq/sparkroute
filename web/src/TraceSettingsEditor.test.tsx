import { useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { TraceSettingsEditor } from "./TraceSettingsEditor";
import type { ConfigurationDocument } from "./types";
afterEach(cleanup);
function Editor(){ const [document,setDocument]=useState<ConfigurationDocument>({providers:[],deployments:[],virtual_models:[]});return <><TraceSettingsEditor document={document} disabled={false} onChange={setDocument}/><output aria-label="Draft">{JSON.stringify(document)}</output></>;}
it("keeps payload capture separate from OTLP and preserves disabled settings",()=>{
 render(<Editor/>);
 fireEvent.change(screen.getByLabelText("Payload capture"),{target:{value:"enabled"}});
 fireEvent.change(screen.getByLabelText("Trace storage path"),{target:{value:"/private/traces"}});
 fireEvent.change(screen.getByLabelText("OTLP trace export"),{target:{value:"enabled"}});
 fireEvent.change(screen.getByLabelText("Collector trace URL"),{target:{value:"http://localhost:4318/v1/traces"}});
 fireEvent.click(screen.getByText("Add export header"));fireEvent.change(screen.getByLabelText("Trace header environment 1"),{target:{value:"COLLECTOR_TOKEN"}});
 fireEvent.change(screen.getByLabelText("Payload capture"),{target:{value:"disabled"}});
 const o=JSON.parse(screen.getByLabelText("Draft").textContent!).observability;
 expect(o.saved_traces).toMatchObject({enabled:false,path:"/private/traces"});expect(o.otlp_traces).toMatchObject({enabled:true,header_env:{Authorization:"COLLECTOR_TOKEN"}});
 fireEvent.change(screen.getByLabelText("Payload capture"),{target:{value:"inherit"}});
 expect(JSON.parse(screen.getByLabelText("Draft").textContent!).observability).not.toHaveProperty("saved_traces");
});
