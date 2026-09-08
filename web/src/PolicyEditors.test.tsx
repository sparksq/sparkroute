import { useState } from "react";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { GuardrailEditor } from "./GuardrailEditor";
import { PIIVirtualModelSection } from "./PIIEditor";
import { DeploymentMetadataEditor, deploymentMetadata } from "./DeploymentMetadataEditor";
import type { JSONObject } from "./extensions";

afterEach(cleanup);
function Editor({kind, initial = {}, disabled = false}: {kind: "guardrails" | "pii" | "metadata"; initial?: JSONObject; disabled?: boolean}) {
 const [model, update] = useState(initial);
 return <>{kind === "guardrails" ? <GuardrailEditor model={model} updateModel={update} disabled={disabled} modelNames={["main","guard"]} />
 : kind === "pii" ? <PIIVirtualModelSection model={model} updateModel={update} disabled={disabled} />
 : <DeploymentMetadataEditor deployment={model} onChange={update} disabled={disabled} />}<output data-testid="value">{JSON.stringify(model)}</output></>;
}
function value() { return JSON.parse(screen.getByTestId("value").textContent!); }

it("edits ordered guardrails, preserves extension fields, and removes streaming with its last check", () => {
 render(<Editor kind="guardrails" initial={{name:"main",guardrails:{custom:"kept",pre:[{name:"existing",model:"guard",extra:7}]}}} />);
 fireEvent.click(screen.getByText("Guardrails"));
 fireEvent.click(screen.getByRole("button",{name:"Add request guardrail"}));
 const second=within(screen.getByRole("article",{name:"Request guardrail 2"}));
 fireEvent.change(second.getByLabelText(/^Instructions/),{target:{value:"Block secrets"}});
 fireEvent.click(second.getByRole("button",{name:"Move request guardrail 2 up"}));
 expect(value().guardrails.pre[0].prompt).toBe("Block secrets");
 expect(value().guardrails.pre[1].extra).toBe(7);
 fireEvent.click(screen.getByRole("button",{name:"Add response guardrail"}));
 fireEvent.click(screen.getByLabelText("Screen streaming responses"));
 fireEvent.change(screen.getByLabelText(/^Screening window/),{target:{value:"2048"}});
 expect(value().guardrails.stream.window_bytes).toBe(2048);
 fireEvent.click(screen.getByRole("button",{name:"Remove response guardrail 1"}));
 expect(value().guardrails.stream).toBeUndefined();
 expect(value().guardrails.custom).toBe("kept");
});

it("configures PII and prevents the empty-entities default trap", () => {
 render(<Editor kind="pii" initial={{privacy:{other:"kept",pii:{mode:"substitute",entities:["email"],custom:true}}}} />);
 fireEvent.click(screen.getByText("PII privacy"));
 expect(screen.getByRole("checkbox",{name:"Email"})).toBeDisabled();
 fireEvent.change(screen.getByLabelText("Caller response"),{target:{value:"masked"}});
 expect(value().privacy.pii).toMatchObject({response:"masked",custom:true});
 fireEvent.click(screen.getByLabelText("Enable PII substitution"));
 expect(value().privacy).toEqual({other:"kept",pii:{mode:"disabled"}});
 fireEvent.click(screen.getByLabelText("Enable PII substitution"));
 expect(value().privacy.pii).toEqual({mode:"substitute"});
});

it("keeps generated policy controls read-only", () => {
 render(<Editor kind="guardrails" disabled initial={{name:"main"}} />);
 fireEvent.click(screen.getByText("Guardrails"));
 expect(screen.getByRole("button",{name:"Add request guardrail"})).toBeDisabled();
});

it("stores explicit free pricing and clears unknown metadata without dropping unrelated fields", () => {
 render(<Editor kind="metadata" initial={{model_metadata:{size_b:32,custom:true},provider:"local"}} />);
 fireEvent.change(screen.getByLabelText("Input price / million tokens"),{target:{value:"0"}});
 fireEvent.change(screen.getByLabelText("Model size (B parameters)"),{target:{value:""}});
 expect(value()).toEqual({provider:"local",model_metadata:{custom:true,input_price:0}});
});

it("previews metadata across generated deployments, aliases, and fallback pools", () => {
 const result=deploymentMetadata({providers:[],deployments:[{name:"paid",model_metadata:{size_b:70,context:8192,input_price:2}}],virtual_models:[{name:"coding",aliases:["code"],pools:[{targets:[{deployment:"paid"}]},{targets:[{deployment:"local"}]}]}]},
 {providers:[],deployments:[{name:"local",model_metadata:{size_b:8,context:65536,input_price:0}}],virtual_models:[]});
 expect(result.code).toEqual({size_b:70,context:8192,input_price:2});
 expect(result.coding).toEqual(result.code);
});
