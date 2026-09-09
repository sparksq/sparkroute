// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from "react";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { PolicyProfilesEditor } from "./PolicyProfilesEditor";
import { VirtualModelEditor } from "./VirtualModelEditor";
import { copyInlineProfile, renameProfile } from "./policyProfiles";
import type { ConfigurationDocument } from "./types";

afterEach(cleanup);
const model = (name: string) => ({name, pools:[{targets:[{deployment:"d",weight:100}]}]});
const generated = {deployments:[{name:"d",provider:"p",model:"upstream"}],virtual_models:[model("coding"),{...model("coding:low"), request_overrides:{chat_completions:{temperature:.2}}}]};
function Harness({page="pii", initial={}, readOnly=generated, disabled=false}: {page?:"pii"|"guardrails"|"models";initial?:ConfigurationDocument;readOnly?:ConfigurationDocument;disabled?:boolean}) {
 const [doc,setDoc]=useState<ConfigurationDocument>({providers:[],deployments:[],virtual_models:[],...initial});
 return <>{page==="models" ? <VirtualModelEditor document={doc} readOnlyDocument={readOnly} disabled={disabled} onChange={setDoc} /> : <PolicyProfilesEditor document={doc} readOnlyDocument={readOnly} kind={page} disabled={disabled} onChange={setDoc} />}<output data-testid="document">{JSON.stringify(doc)}</output></>;
}
const value=()=>JSON.parse(screen.getByTestId("document").textContent!);
it("creates a shared PII profile, assigns a generated model, and renames without losing references",()=>{
 render(<Harness />);
 fireEvent.click(screen.getByRole("button",{name:"Add pii privacy profile"}));
 fireEvent.change(screen.getByLabelText("Caller response"),{target:{value:"masked"}});
 fireEvent.click(screen.getByRole("button",{name:"Apply profile"}));
 expect(value().model_policies.coding.pii_profile).toBe("pii-policy");
 expect(value().virtual_models).toEqual([]);
 expect(screen.getByRole("button",{name:"Delete profile"})).toBeDisabled();
 fireEvent.change(screen.getByLabelText("Profile name"),{target:{value:"personal"}});
 fireEvent.click(screen.getByRole("button",{name:"Rename"}));
 expect(value().pii_profiles.personal.response).toBe("masked");
 expect(value().pii_profiles["pii-policy"]).toBeUndefined();
 expect(value().model_policies.coding.pii_profile).toBe("personal");
 fireEvent.click(screen.getByRole("button",{name:"Remove assignment for coding"}));
 fireEvent.click(screen.getByRole("button",{name:"Delete profile"}));
 fireEvent.click(screen.getByRole("button",{name:"Confirm delete profile"}));
 expect(value().pii_profiles).toEqual({});
});
it("keeps generated settings disabled while allowing policy assignments on the parent model",()=>{
 const before=JSON.stringify(generated);
 render(<Harness page="models" initial={{pii_profiles:{personal:{mode:"substitute"}},guardrail_profiles:{safe:{}}}} />);
 expect(screen.getByLabelText("Canonical name")).toBeDisabled();
 expect(within(screen.getByRole("complementary",{name:"Virtual models"})).queryByRole("button",{name:/coding:low/})).not.toBeInTheDocument();
 expect(screen.getByLabelText("PII privacy profile")).toBeEnabled();
 fireEvent.change(screen.getByLabelText("PII privacy profile"),{target:{value:"profile:personal"}});
 fireEvent.change(screen.getByLabelText("Guardrail profile"),{target:{value:"profile:safe"}});
 expect(value().model_policies.coding).toEqual({pii_profile:"personal",guardrail_profile:"safe"});
 expect(value().virtual_models).toEqual([]);expect(JSON.stringify(generated)).toBe(before);
 fireEvent.change(screen.getByLabelText("PII privacy profile"),{target:{value:"disabled"}});
 expect(value().model_policies.coding.pii_profile).toBe("");
});
it("blocks assignment changes for a read-only operator and preserves dormant assignments",()=>{
 render(<Harness disabled initial={{pii_profiles:{personal:{}},model_policies:{gone:{pii_profile:"personal"}}}} />);
 expect(screen.getByText("Not currently available · assignment retained")).toBeVisible();
 expect(screen.getByRole("button",{name:"Delete profile"})).toBeDisabled();
 expect(screen.getByRole("button",{name:"Remove assignment for gone"})).toBeDisabled();
 expect(screen.getByRole("button",{name:"Apply profile"})).toBeDisabled();
});
it("configures guardrail policies using the complete model namespace",()=>{
 render(<Harness page="guardrails" initial={{virtual_models:[{...model("guard"),visibility:"hidden",aliases:["safety"]}]}} />);
 fireEvent.click(screen.getByRole("button",{name:"Add guardrail profile"}));
 fireEvent.click(screen.getByRole("button",{name:"Add request guardrail"}));
 fireEvent.change(screen.getByLabelText(/^Guardrail model/),{target:{value:"safety"}});
 fireEvent.change(screen.getByLabelText(/^Instructions/),{target:{value:"Block secrets"}});
 expect(value().guardrail_profiles["guardrail-policy"].pre[0]).toMatchObject({model:"safety",prompt:"Block secrets"});
});
it("preserves legacy inline policies when converting and carries an assignment through model rename",()=>{
 const legacy={...model("chat"),privacy:{pii:{mode:"substitute",entities:["email"],response:"masked"}}};
 const initial=copyInlineProfile({virtual_models:[legacy,{...model("chat:low"),request_overrides:{chat_completions:{temperature:.2}}}],deployments:generated.deployments},legacy,"pii");
 expect((initial.virtual_models as unknown[])[0]).toEqual(legacy);
 const renamed=renameProfile(initial,"pii","pii-policy","renamed");
 render(<Harness page="models" initial={renamed} readOnly={{}} />);
 fireEvent.change(screen.getByLabelText("Canonical name"),{target:{value:"chat-v2"}});
 expect(value().model_policies["chat-v2"].pii_profile).toBe("renamed");
 expect(value().model_policies.chat).toBeUndefined();
 expect(value().virtual_models[0].privacy).toEqual(legacy.privacy);
 expect(value().virtual_models[1].name).toBe("chat-v2:low");
});
