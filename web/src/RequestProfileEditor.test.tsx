import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { RequestProfileEditor } from "./RequestProfileEditor";
afterEach(cleanup);
const base = {name:"coding", aliases:["code"], pools:[{priority:0, targets:[{deployment:"one-job", weight:1}]}]};
it("creates an operator profile over the same deployment without duplicating aliases", () => {
 const add = vi.fn(); render(<RequestProfileEditor model={base} names={["coding","code"]} disabled={false} readOnly={true} onAdd={add} onChange={() => {}} />);
 fireEvent.click(screen.getByText("Add a named profile"));
 fireEvent.change(screen.getByLabelText(/Profile suffix/), {target:{value:"xhigh"}});
 fireEvent.change(screen.getByLabelText("Request parameters (JSON)"), {target:{value:'{"reasoning_effort":"xhigh"}'}});
 fireEvent.click(screen.getByText("Add profile to draft"));
 expect(add).toHaveBeenCalledWith({...base, name:"coding:xhigh", aliases:[], request_overrides:{chat_completions:{reasoning_effort:"xhigh"}}});
});
it("rejects a conflicting name and preserves unsubmitted values", () => {
 const add = vi.fn(); render(<RequestProfileEditor model={base} names={["coding:low"]} disabled={false} readOnly={false} onAdd={add} onChange={() => {}} />);
 fireEvent.click(screen.getByText("Add a named profile")); fireEvent.click(screen.getByText("Add profile to draft"));
 expect(add).not.toHaveBeenCalled(); expect(screen.getByRole("alert")).toHaveTextContent("unique profile");
});
