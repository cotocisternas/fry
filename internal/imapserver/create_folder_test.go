//go:build linux

package imapserver

import "testing"

func TestIMAPCreateTrailingHierarchyDelimiter(t *testing.T) {
	f := setup(t)
	c := f.client(t)
	{
		err := c.Create("Projects/", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := c.Select("Projects", nil).Wait()
		requireTestCondition(t, err == nil, "CREATE retained trailing hierarchy delimiter", err)
	}
	listed, err := c.List("", "Projects*", nil).Collect()
	requireTestCondition(
		t,
		(((err == nil && len(listed) == 1) && listed[0].Mailbox == "Projects") && len(listed[0].Attrs) == 0),
		"created mailbox is not a single selectable hierarchy entry",
		listed,
		err,
	)
	{
		err := c.Create("Projects", nil).Wait()
		requireTestCondition(t, err != nil, "normalized duplicate CREATE succeeded")
	}
	{
		err := c.Create("/", nil).Wait()
		requireTestCondition(t, err != nil, "separator-only CREATE succeeded")
	}
	{
		err := c.Create("INBOX/", nil).Wait()
		requireTestCondition(t, err != nil, "trailing delimiter bypassed existing INBOX")
	}
}
